package apps

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
	"github.com/MightyX3N/zeroos/internal/storage"
)

// Backups.
//
// A backup is one file on the NAS: the app's own data from the SSD, the recipe
// it runs from and its settings. Next to it sits a small description, written
// last, so a half-written backup is never listed. Nothing about backups is
// kept in ZeroOS's own database: the files are the truth, so they can be put
// back on a freshly installed mini PC too.
//
// Folders on the NAS are not part of a backup; they are on the NAS already.

// Backup describes one backup file.
type Backup struct {
	File     string                  `json:"file"`
	AppID    string                  `json:"appId"`
	Name     string                  `json:"name"`
	StoreID  string                  `json:"storeId"`
	Catalog  string                  `json:"catalog"`
	Version  string                  `json:"version"`
	At       int64                   `json:"at"`
	Size     int64                   `json:"size"`     // bytes on the NAS
	DataSize int64                   `json:"dataSize"` // bytes once put back
	Auto     bool                    `json:"auto"`
	Port     int                     `json:"port"`
	Folders  map[string]BackupFolder `json:"folders"`
	Env      map[string]string       `json:"env"`
	GPU      bool                    `json:"gpu"`
	ZeroOS   string                  `json:"zeroos"`
}

// BackupFolder is a picked folder, with the share's name so it can be found
// again on a mini PC where the shares were added anew.
type BackupFolder struct {
	Picked
	RootName string `json:"rootName"`
}

// BackupPlan is where and when backups are made.
type BackupPlan struct {
	Root      string `json:"root"`
	Path      string `json:"path"`
	Label     string `json:"label"`     // "Media (NAS) / ZeroOS backups"
	Set       bool   `json:"set"`       // a place was chosen
	Connected bool   `json:"connected"` // and it can be reached now
	OnSSD     bool   `json:"onSsd"`     // the place is on this mini PC itself
	When      string `json:"when"`      // off, daily, weekly
	Hour      int    `json:"hour"`
	Keep      int    `json:"keep"`
	Last      int64  `json:"last"`    // when the automatic backups last ran
	Problem   string `json:"problem"` // what went wrong the last time
	Running   bool   `json:"running"`
}

const backupState = "backup" // the app's state while a backup is made or put back

var (
	backupFileRe = regexp.MustCompile(`^\d{8}-\d{6}(-auto)?\.tar\.gz$`)
	appIDRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,80}$`)
	now          = time.Now
)

// Plan reads where and when backups are made.
func (m *Manager) Plan() BackupPlan {
	p := BackupPlan{Root: m.c.Setting("backup.root"), Path: m.c.Setting("backup.path"), When: m.c.Setting("backup.when"), Hour: 3, Keep: 7, Problem: m.c.Setting("backup.problem")}
	if n, err := strconv.Atoi(m.c.Setting("backup.hour")); err == nil && n >= 0 && n < 24 {
		p.Hour = n
	}
	if n, err := strconv.Atoi(m.c.Setting("backup.keep")); err == nil && n >= 1 && n <= 60 {
		p.Keep = n
	}
	p.Last, _ = strconv.ParseInt(m.c.Setting("backup.last"), 10, 64)
	if p.When != "off" && p.When != "weekly" {
		p.When = "daily"
	}
	for _, r := range m.st.Roots() {
		if r.ID == p.Root && p.Root != "" {
			p.Set, p.Connected, p.OnSSD = true, r.Connected, r.ID == "local"
			p.Label = r.Label + strings.ReplaceAll(filepath.Clean("/"+p.Path), "/", " / ")
			p.Label = strings.TrimSuffix(p.Label, " / ")
		}
	}
	p.Running = m.backupsRunning.Load()
	return p
}

// SetPlan saves where and when backups are made.
func (m *Manager) SetPlan(root, path, when string, hour, keep int) error {
	if _, err := m.st.Resolve(root, path); err != nil || root == "" {
		return errors.New("Choose a folder for the backups.")
	}
	if !m.st.RootConnected(root) {
		return errors.New("That NAS share isn't connected right now.")
	}
	if when != "off" && when != "daily" && when != "weekly" {
		return errors.New("Choose how often backups are made.")
	}
	if hour < 0 || hour > 23 || keep < 1 || keep > 60 {
		return errors.New("That time or number isn't possible.")
	}
	dir, _ := m.st.Resolve(root, path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errors.New("ZeroOS can't write in that folder.")
	}
	probe := filepath.Join(dir, ".zeroos-write-test")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		return errors.New("ZeroOS can't write in that folder.")
	}
	_ = os.Remove(probe)
	for k, v := range map[string]string{"backup.root": root, "backup.path": filepath.Clean("/" + path), "backup.when": when, "backup.hour": strconv.Itoa(hour), "backup.keep": strconv.Itoa(keep), "backup.problem": ""} {
		if err := m.c.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

var errNoBackupPlace = errors.New("Choose where backups go first, in Settings under Backups.")

// backupDir is the folder all backups live in.
func (m *Manager) backupDir() (string, error) {
	p := m.Plan()
	if !p.Set {
		return "", errNoBackupPlace
	}
	gone := errors.New("The place for backups isn't connected right now.")
	if !p.Connected {
		return "", gone
	}
	// Ask the system itself too: writing into the empty folder a share is
	// normally mounted on would fill the SSD instead of the NAS.
	for _, r := range m.st.Roots() {
		if r.ID == p.Root && r.ID != "local" && !storage.IsMounted(r.Path) {
			return "", gone
		}
	}
	return m.st.Resolve(p.Root, p.Path)
}

func readBackup(dir, file string) (*Backup, error) {
	if !backupFileRe.MatchString(file) {
		return nil, errors.New("That backup doesn't exist.")
	}
	b, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(file, ".tar.gz")+".json"))
	if err != nil {
		return nil, errors.New("That backup doesn't exist any more.")
	}
	var bk Backup
	if json.Unmarshal(b, &bk) != nil || !appIDRe.MatchString(bk.AppID) {
		return nil, errors.New("That backup can't be read.")
	}
	st, err := os.Stat(filepath.Join(dir, file))
	if err != nil {
		return nil, errors.New("That backup doesn't exist any more.")
	}
	bk.File, bk.Size = file, st.Size()
	return &bk, nil
}

func listBackupDir(dir string) []Backup {
	out := []Backup{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if bk, err := readBackup(dir, e.Name()); err == nil {
			out = append(out, *bk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File > out[j].File })
	return out
}

// Backups lists one app's backups, newest first.
func (m *Manager) Backups(appID string) []Backup {
	dir, err := m.backupDir()
	if err != nil || !appIDRe.MatchString(appID) {
		return []Backup{}
	}
	return listBackupDir(filepath.Join(dir, appID))
}

// AllBackups lists every app's backups, including those of apps that are no longer installed.
func (m *Manager) AllBackups() map[string][]Backup {
	out := map[string][]Backup{}
	dir, err := m.backupDir()
	if err != nil {
		return out
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && appIDRe.MatchString(e.Name()) {
			if l := listBackupDir(filepath.Join(dir, e.Name())); len(l) > 0 {
				out[e.Name()] = l
			}
		}
	}
	return out
}

// DeleteBackup removes one backup file.
func (m *Manager) DeleteBackup(appID, file string) error {
	dir, err := m.backupDir()
	if err != nil {
		return err
	}
	if !appIDRe.MatchString(appID) || !backupFileRe.MatchString(file) {
		return errors.New("That backup doesn't exist.")
	}
	dir = filepath.Join(dir, appID)
	_ = os.Remove(filepath.Join(dir, strings.TrimSuffix(file, ".tar.gz")+".json"))
	if err := os.Remove(filepath.Join(dir, file)); err != nil && !os.IsNotExist(err) {
		return errors.New("Couldn't delete that backup.")
	}
	_ = os.Remove(dir) // only goes when it is empty
	return nil
}

// SetAutoBackup includes an app in the automatic backups, or leaves it out.
func (m *Manager) SetAutoBackup(id string, on bool) error {
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	a.Settings.NoBackup = !on
	m.saveSettings(a)
	return nil
}

type countWriter struct {
	w io.Writer
	n atomic.Int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// tarOK tells whether tar finished well enough: 1 only means it had remarks
// (a socket it skipped, for example).
func tarOK(err error) bool {
	var ee *exec.ExitError
	return err == nil || (errors.As(err, &ee) && ee.ExitCode() == 1)
}

func canBackUp(a *App) error {
	switch a.State {
	case "installing", "failed", "removing", "updating", backupState:
		return errors.New("A backup can't be made while the app is " + map[string]string{"installing": "installing", "failed": "not installed properly", "removing": "being removed", "updating": "updating", backupState: "busy with a backup"}[a.State] + ".")
	}
	return nil
}

// MakeBackup starts a backup of one app.
func (m *Manager) MakeBackup(id string, auto bool) (*Job, error) {
	a, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if err := canBackUp(a); err != nil {
		return nil, err
	}
	if _, err := m.backupDir(); err != nil {
		return nil, err
	}
	if !m.lock(id) {
		return nil, ErrBusy
	}
	job := m.newJob(id)
	go func() {
		defer m.unlock(id)
		_, err := m.runBackup(job, a, auto)
		if err == nil {
			job.step("Done")
		}
		job.finish(err)
	}()
	return job, nil
}

// runBackup stops the app, writes its backup and starts it again. The caller holds the app's lock.
func (m *Manager) runBackup(job *Job, a *App, auto bool) (*Backup, error) {
	id := a.ID
	fail := func(why string, err error) (*Backup, error) {
		detail := ""
		if err != nil {
			detail = err.Error()
			job.log(detail)
		}
		m.c.Log("system", "backup failed", a.Name, id, false, strings.TrimSpace(why+" "+detail))
		return nil, errors.New(why)
	}
	root, err := m.backupDir()
	if err != nil {
		return fail(err.Error(), nil)
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail("ZeroOS can't write in the folder for backups.", err)
	}
	if err := m.ensureRecipe(a); err != nil {
		return fail("The app's recipe is missing, so there is nothing to back up.", err)
	}
	appDir := m.appDir(id)
	dataDir := filepath.Join(appDir, "data")
	_ = os.MkdirAll(dataDir, 0o755)
	size := dirSize(dataDir)
	if free := freeSpace(dir); free < size+50<<20 {
		return fail(fmt.Sprintf("There isn't enough room for the backup (it needs up to %s, %s is free).", humanBytes(size), humanBytes(free)), nil)
	}

	wasState := a.State
	m.setState(id, backupState)
	again := func() {
		if a.Settings.Desired != "running" || wasState == "waiting" {
			m.setState(id, wasState)
			return
		}
		job.step("Starting it again")
		if err := m.compose(id, job, "up", "-d", "--remove-orphans"); err != nil {
			m.setState(id, "crashed")
			return
		}
		m.setState(id, "running")
	}
	if _, err := os.Stat(filepath.Join(appDir, "docker-compose.yml")); err == nil {
		job.step("Stopping it for a moment")
		if err := m.compose(id, job, "stop"); err != nil {
			m.setState(id, wasState)
			return fail("Couldn't stop the app, so no backup was made.", err)
		}
		_ = m.hook(id, "stop", nil)
	}

	at := now()
	bk := &Backup{AppID: id, Name: a.Name, StoreID: a.StoreID, Catalog: a.Catalog, Version: a.Settings.Version, At: at.Unix(), DataSize: size, Auto: auto,
		Port: a.Port, Folders: map[string]BackupFolder{}, Env: a.Settings.Env, GPU: a.Settings.GPU, ZeroOS: core.Version}
	roots := m.st.Roots()
	for k, p := range a.Folders {
		f := BackupFolder{Picked: p}
		for _, r := range roots {
			if r.ID == p.Root {
				f.RootName = r.Label
			}
		}
		bk.Folders[k] = f
	}
	stem := at.Format("20060102-150405")
	if auto {
		stem += "-auto"
	}
	for i := 0; ; i++ { // two in the same second
		if _, err := os.Stat(filepath.Join(dir, stem+".tar.gz")); err != nil {
			break
		}
		at = at.Add(time.Second)
		stem = at.Format("20060102-150405")
		if auto {
			stem += "-auto"
		}
	}
	bk.File = stem + ".tar.gz"
	desc, _ := json.MarshalIndent(bk, "", "  ")
	tmp := filepath.Join(appDir, "backup.tmp")
	_ = os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		again()
		return fail("Couldn't prepare the backup.", err)
	}
	_ = os.WriteFile(filepath.Join(tmp, "backup.json"), desc, 0o600)

	job.step("Copying it to " + m.Plan().Label)
	part := filepath.Join(dir, bk.File+".part")
	err = func() error {
		f, err := os.Create(part)
		if err != nil {
			return err
		}
		defer f.Close()
		cw := &countWriter{w: f}
		gz, _ := gzip.NewWriterLevel(cw, gzip.BestSpeed)
		cmd := exec.Command("tar", "--numeric-owner", "-cpf", "-", "-C", tmp, "backup.json", "-C", appDir, "recipe", "data")
		in := &countWriter{w: gz}
		cmd.Stdout = in
		var errOut strings.Builder
		cmd.Stderr = &errOut
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Second):
					job.mu.Lock()
					job.Step = fmt.Sprintf("Copying it to the backups folder (%s of about %s)", humanBytes(in.n.Load()), humanBytes(size))
					job.mu.Unlock()
				}
			}
		}()
		runErr := cmd.Run()
		close(stop)
		if s := strings.TrimSpace(errOut.String()); s != "" {
			job.log(s)
		}
		if !tarOK(runErr) {
			return fmt.Errorf("tar: %v", runErr)
		}
		if err := gz.Close(); err != nil {
			return err
		}
		return f.Sync()
	}()
	if err == nil {
		err = os.Rename(part, filepath.Join(dir, bk.File))
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, stem+".json"), desc, 0o600)
	}
	if err != nil {
		_ = os.Remove(part)
		_ = os.Remove(filepath.Join(dir, bk.File))
		again()
		return fail("Writing the backup didn't work. Check that the place for backups is connected and has room.", err)
	}
	again()
	if st, err := os.Stat(filepath.Join(dir, bk.File)); err == nil {
		bk.Size = st.Size()
	}
	if auto {
		m.pruneBackups(id)
	}
	m.c.Log("system", "made a backup of", a.Name, id, true, "")
	return bk, nil
}

// pruneBackups deletes the oldest automatic backups of an app beyond the number to keep.
// Backups made by hand stay until someone deletes them.
func (m *Manager) pruneBackups(id string) {
	keep, n := m.Plan().Keep, 0
	for _, b := range m.Backups(id) {
		if !b.Auto {
			continue
		}
		if n++; n > keep {
			_ = m.DeleteBackup(id, b.File)
		}
	}
}

// unpack puts a backup file's contents into dir.
func unpack(file, dir string, job *Job) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cmd := exec.Command("tar", "--numeric-owner", "-xpf", "-", "-C", dir)
	cmd.Stdin = gz
	out, err := cmd.CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" && job != nil {
		job.log(s)
	}
	if err != nil {
		return fmt.Errorf("tar: %v", err)
	}
	if !dirExists(filepath.Join(dir, "data")) || !dirExists(filepath.Join(dir, "recipe")) {
		return errors.New("the backup file is incomplete")
	}
	return nil
}

// foldersFrom finds a backup's folders on this mini PC: by share id, else by
// the share's name. missing names a share that isn't here.
func (m *Manager) foldersFrom(bk *Backup) (out map[string]Picked, missing string) {
	out = map[string]Picked{}
	roots := m.st.Roots()
	for k, f := range bk.Folders {
		found := false
		for _, r := range roots { // the same share, by name
			if r.Label == f.RootName {
				out[k], found = Picked{Root: r.ID, Path: f.Path}, true
			}
		}
		for _, r := range roots {
			if !found && f.RootName == "" && r.ID == f.Root {
				out[k], found = f.Picked, true
			}
		}
		if !found {
			missing = f.RootName
			if missing == "" {
				missing = f.Root
			}
		}
	}
	return out, missing
}

// RestoreBackup puts a backup back into an installed app. How the app is now
// is kept for a week, like before an update, so it can be undone.
func (m *Manager) RestoreBackup(id, file string) (*Job, error) {
	a, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if err := canBackUp(a); err != nil {
		return nil, errors.New(strings.Replace(err.Error(), "A backup can't be made", "A backup can't be put back", 1))
	}
	root, err := m.backupDir()
	if err != nil {
		return nil, err
	}
	bk, err := readBackup(filepath.Join(root, id), file)
	if err != nil {
		return nil, err
	}
	if free := freeSpace(m.appDir(id)); free < bk.DataSize+bk.DataSize/10+200<<20 {
		return nil, fmt.Errorf("There isn't enough free space on the SSD to put this backup back (it needs %s, %s is free).", humanBytes(bk.DataSize+bk.DataSize/10+200<<20), humanBytes(free))
	}
	if !m.lock(id) {
		return nil, ErrBusy
	}
	job := m.newJob(id)
	go func() {
		defer m.unlock(id)
		m.runRestore(job, a, bk, filepath.Join(root, id, file))
	}()
	return job, nil
}

func (m *Manager) runRestore(job *Job, a *App, bk *Backup, file string) {
	id := a.ID
	appDir := m.appDir(id)
	dataDir, recipe, undo, tmp := filepath.Join(appDir, "data"), m.recipeDir(id), m.undoDir(id), filepath.Join(appDir, "restore.tmp")
	defer os.RemoveAll(tmp)
	wasState, old := "running", a.Settings.Version
	if a.Settings.Desired != "running" {
		wasState = "stopped"
	}
	before := a.State
	m.setState(id, backupState)

	job.step("Reading the backup")
	if err := unpack(file, tmp, job); err != nil {
		m.setState(id, before)
		m.c.Log("system", "putting a backup back failed", a.Name, id, false, err.Error())
		job.finish(errors.New("The backup couldn't be read, so nothing was changed."))
		return
	}
	job.step("Stopping it")
	_ = m.compose(id, job, "down", "--remove-orphans")
	_ = m.hook(id, "stop", nil)

	// how it is now moves aside, for undoing
	job.step("Keeping how it is now, to go back to")
	m.dropKeptImages(id)
	_ = os.RemoveAll(undo)
	err := os.MkdirAll(undo, 0o755)
	if err == nil {
		err = copyRecipe(recipe, filepath.Join(undo, "recipe"))
	}
	if err == nil {
		was, _ := json.Marshal(appBefore{Name: a.Name, Port: a.Port, Folders: a.Folders, Env: a.Settings.Env})
		err = os.WriteFile(filepath.Join(undo, "app.json"), was, 0o600)
	}
	if err == nil {
		err = os.Rename(dataDir, filepath.Join(undo, "data"))
	}
	if err != nil {
		_ = os.RemoveAll(undo)
		job.log(err.Error())
		m.startAgain(id, job, wasState)
		job.finish(errors.New("Couldn't keep how the app is now, so the backup was not put back."))
		return
	}
	a.Settings.Undo = &UndoPoint{Version: old, At: now().Unix(), Size: dirSize(filepath.Join(undo, "data")), Restore: true}
	m.saveSettings(a)

	fail := func(why string) {
		job.log("✗ " + why)
		if err := m.putBack(id, job, a, wasState, false); err != nil {
			m.setState(id, "failed")
			m.c.Log("system", "putting a backup back failed", a.Name, id, false, why+" Going back to how it was failed too.")
			job.finish(errors.New(why + " Going back to how it was didn't work either: " + err.Error()))
			return
		}
		m.c.Log("system", "putting a backup back failed, the app is as it was", a.Name, id, false, why)
		job.finish(errors.New(why + " The app is back exactly as it was before."))
	}
	job.step("Putting the backup in place")
	if err := os.Rename(filepath.Join(tmp, "data"), dataDir); err != nil {
		_ = os.MkdirAll(dataDir, 0o755)
		fail("The backup couldn't be put in place.")
		return
	}
	if err := copyRecipe(filepath.Join(tmp, "recipe"), recipe); err != nil {
		fail("The backup has no recipe for the app.")
		return
	}
	folders, _ := m.foldersFrom(bk)
	for k := range a.Folders { // a folder whose share isn't here stays where it points now
		if p, ok := folders[k]; ok {
			a.Folders[k] = p
		}
	}
	if bk.Env != nil {
		a.Settings.Env = bk.Env
	}
	a.Settings.Version = recipeVersion(recipe)
	m.saveSettings(a)
	if err := m.writeCompose(id, false); err != nil {
		fail("The backup couldn't be prepared.")
		return
	}
	job.step("Starting it")
	if err := m.compose(id, job, "up", "-d", "--remove-orphans"); err != nil {
		fail("The app didn't start from the backup.")
		return
	}
	job.step("Checking that it works")
	if !m.comesUp(a) {
		fail("The app didn't start properly from the backup.")
		return
	}
	if wasState == "stopped" {
		_ = m.compose(id, nil, "stop")
		_ = m.hook(id, "stop", nil)
	}
	m.setState(id, wasState)
	m.mu.Lock()
	delete(m.frame, id)
	m.newer[id] = false
	m.mu.Unlock()
	m.c.Log("system", "put a backup back into", fmt.Sprintf("%s (from %s)", a.Name, time.Unix(bk.At, 0).Format("2 Jan 2006 15:04")), id, true, "")
	job.step("Done")
	job.finish(nil)
}

// InstallBackup brings back an app that isn't installed, from one of its backups.
func (m *Manager) InstallBackup(appID, file string, userID int64) (*Job, error) {
	root, err := m.backupDir()
	if err != nil {
		return nil, err
	}
	if !appIDRe.MatchString(appID) {
		return nil, errors.New("That backup doesn't exist.")
	}
	bk, err := readBackup(filepath.Join(root, appID), file)
	if err != nil {
		return nil, err
	}
	id := bk.AppID
	if _, err := m.Get(id); err == nil {
		return nil, errors.New(bk.Name + " is installed. Put the backup back from its settings instead.")
	}
	folders, missing := m.foldersFrom(bk)
	if missing != "" {
		return nil, fmt.Errorf("This app keeps files on \"%s\", which isn't added to ZeroOS. Add that NAS share first, with the same name.", missing)
	}
	for _, p := range folders {
		if !m.st.RootConnected(p.Root) {
			return nil, errors.New("A NAS share this app uses isn't connected right now.")
		}
	}
	if free := freeSpace(m.c.Cfg.DataDir); free < bk.DataSize+bk.DataSize/10+200<<20 {
		return nil, fmt.Errorf("There isn't enough free space on the SSD to put this backup back (it needs %s, %s is free).", humanBytes(bk.DataSize+bk.DataSize/10+200<<20), humanBytes(free))
	}
	port := bk.Port
	if l, err := net.Listen("tcp", fmt.Sprintf(":%d", port)); err != nil || port <= 0 || m.portTaken(port, id) {
		if port, err = m.freePort(); err != nil {
			return nil, err
		}
	} else {
		l.Close()
	}
	settings, _ := json.Marshal(AppSettings{Env: bk.Env, GPU: bk.GPU && dirExists("/dev/dri"), Desired: "running", Version: bk.Version})
	fj, _ := json.Marshal(folders)
	if _, err := m.c.DB.Exec(`INSERT INTO apps(id,store_id,catalog,name,port,state,settings,folders,installed_by,created) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, bk.StoreID, bk.Catalog, bk.Name, port, "installing", string(settings), string(fj), userID, now().Unix()); err != nil {
		return nil, err
	}
	m.lock(id)
	job := m.newJob(id)
	go func() {
		defer m.unlock(id)
		fail := func(why string, err error) {
			if err != nil {
				job.log(err.Error())
			}
			m.setState(id, "failed")
			m.c.Log("system", "putting a backup back failed", bk.Name, id, false, why)
			job.finish(errors.New(why))
		}
		appDir := m.appDir(id)
		tmp := filepath.Join(appDir, "restore.tmp")
		defer os.RemoveAll(tmp)
		job.step("Reading the backup")
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			fail("Couldn't prepare the app's folder.", err)
			return
		}
		if err := unpack(filepath.Join(root, appID, file), tmp, job); err != nil {
			fail("The backup couldn't be read.", err)
			return
		}
		job.step("Putting the backup in place")
		dataDir := filepath.Join(appDir, "data")
		if dirExists(dataDir) { // data left behind when the app was removed
			aside := filepath.Join(appDir, "data.before-backup")
			_ = os.RemoveAll(aside)
			_ = os.Rename(dataDir, aside)
		}
		_ = os.RemoveAll(m.undoDir(id))
		_ = os.RemoveAll(m.recipeDir(id))
		if err := os.Rename(filepath.Join(tmp, "data"), dataDir); err != nil {
			fail("The backup couldn't be put in place.", err)
			return
		}
		if err := copyRecipe(filepath.Join(tmp, "recipe"), m.recipeDir(id)); err != nil {
			fail("The backup has no recipe for the app.", err)
			return
		}
		if err := m.writeCompose(id, false); err != nil {
			fail("The app couldn't be prepared.", err)
			return
		}
		job.step("Downloading")
		if err := m.compose(id, job, "pull", "--ignore-buildable"); err != nil {
			if err2 := m.compose(id, job, "pull"); err2 != nil && !m.imagesPresent(id) {
				fail("Couldn't download the app. Check that this computer is online.", nil)
				return
			}
		}
		job.step("Starting")
		if err := m.compose(id, job, "up", "-d", "--remove-orphans"); err != nil {
			fail("The app didn't start. See the details below.", nil)
			return
		}
		job.step("Checking that it works")
		a, err := m.Get(id)
		if err != nil || !m.comesUp(a) {
			fail("The app didn't start properly from the backup.", nil)
			return
		}
		m.setState(id, "running")
		m.c.Log("system", "brought back from a backup", bk.Name, id, true, "")
		job.step("Done")
		job.finish(nil)
	}()
	return job, nil
}

// BackUpAll backs up every app, one after the other. With auto, apps left out
// of the automatic backups are skipped. It returns how many worked and failed.
func (m *Manager) BackUpAll(auto bool) (done, failed int, err error) {
	if !m.backupsRunning.CompareAndSwap(false, true) {
		return 0, 0, errors.New("Backups are being made already.")
	}
	defer m.backupsRunning.Store(false)
	if _, err := m.backupDir(); err != nil {
		if auto {
			_ = m.c.SetSetting("backup.problem", "The automatic backups weren't made: the place for backups wasn't connected.")
			m.c.Log("system", "automatic backups not made", "", "", false, err.Error())
		}
		return 0, 0, err
	}
	list, err := m.List()
	if err != nil {
		return 0, 0, err
	}
	var names []string
	for _, a := range list {
		if (auto && a.Settings.NoBackup) || canBackUp(a) != nil {
			continue
		}
		if !m.lock(a.ID) {
			failed++
			names = append(names, a.Name)
			continue
		}
		_, err := m.runBackup(&Job{AppID: a.ID}, a, auto)
		m.unlock(a.ID)
		if err != nil {
			failed++
			names = append(names, a.Name)
		} else {
			done++
		}
	}
	problem := ""
	if failed > 0 {
		problem = "The last backup didn't work for: " + strings.Join(names, ", ") + ". The Activity list says why."
	}
	_ = m.c.SetSetting("backup.problem", problem)
	_ = m.c.SetSetting("backup.last", strconv.FormatInt(now().Unix(), 10))
	return done, failed, nil
}

// BackupTick makes the automatic backups when it is time. Call it every minute or so.
func (m *Manager) BackupTick() {
	p := m.Plan()
	if !p.Set || p.When == "off" || p.Running {
		return
	}
	t := now()
	today := t.Format("2006-01-02")
	if t.Hour() != p.Hour || m.c.Setting("backup.tried") == today || (p.When == "weekly" && t.Weekday() != time.Sunday) {
		return
	}
	_ = m.c.SetSetting("backup.tried", today)
	go func() { _, _, _ = m.BackUpAll(true) }()
}
