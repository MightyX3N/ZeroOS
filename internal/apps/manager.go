package apps

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
	"github.com/MightyX3N/zeroos/internal/storage"
)

// App is an installed app.
type App struct {
	ID          string            `json:"id"`
	StoreID     string            `json:"storeId"`
	Catalog     string            `json:"catalog"`
	Name        string            `json:"name"`
	Port        int               `json:"-"`
	State       string            `json:"state"` // installing, running, starting, stopped, waiting, failed, crashed, removing
	Problem     string            `json:"problem,omitempty"`
	Settings    AppSettings       `json:"settings"`
	Folders     map[string]Picked `json:"folders"`
	InstalledBy int64             `json:"installedBy"`
	Created     int64             `json:"created"`
	NoGUI       bool              `json:"noGui"`
	URLSuffix   string            `json:"-"`
}

// AppSettings are the answers given at install time plus ZeroOS options.
type AppSettings struct {
	Env     map[string]string `json:"env"`
	GPU     bool              `json:"gpu"`
	Desired string            `json:"desired"`           // running or stopped
	Version string            `json:"version,omitempty"` // the version it runs
	Undo    *UndoPoint        `json:"undo,omitempty"`    // copy kept for undoing the last update
}

// Picked is a folder chosen in the folder picker.
type Picked struct {
	Root string `json:"root"` // "local" or "nas:<id>"
	Path string `json:"path"`
}

type Manager struct {
	c   *core.Core
	cat *Catalog
	st  *storage.Storage

	mu    sync.Mutex
	jobs  map[string]*Job
	frame map[string]bool
	busy  map[string]bool
}

func NewManager(c *core.Core, cat *Catalog, st *storage.Storage) *Manager {
	return &Manager{c: c, cat: cat, st: st, jobs: map[string]*Job{}, frame: map[string]bool{}, busy: map[string]bool{}}
}

func (m *Manager) appDir(id string) string { return filepath.Join(m.c.Cfg.AppsDir(), id) }
func project(id string) string             { return "zeroos-" + id }

// --- jobs (install progress) -----------------------------------------------

type Job struct {
	ID    string   `json:"id"`
	AppID string   `json:"appId"`
	Lines []string `json:"lines"`
	Step  string   `json:"step"`
	Done  bool     `json:"done"`
	Error string   `json:"error,omitempty"`
	mu    sync.Mutex
}

func (j *Job) log(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Lines = append(j.Lines, s)
	if len(j.Lines) > 400 {
		j.Lines = j.Lines[len(j.Lines)-400:]
	}
}

func (j *Job) step(s string) {
	j.mu.Lock()
	j.Step = s
	j.mu.Unlock()
	j.log("» " + s)
}

func (m *Manager) Job(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, false
	}
	j.mu.Lock()
	cp := &Job{ID: j.ID, AppID: j.AppID, Lines: append([]string{}, j.Lines...), Step: j.Step, Done: j.Done, Error: j.Error}
	j.mu.Unlock()
	return cp, true
}

// --- reading installed apps ------------------------------------------------

func (m *Manager) scan(row interface{ Scan(...any) error }) (*App, error) {
	a := &App{}
	var settings, folders string
	if err := row.Scan(&a.ID, &a.StoreID, &a.Catalog, &a.Name, &a.Port, &a.State, &settings, &folders, &a.InstalledBy, &a.Created); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(settings), &a.Settings)
	_ = json.Unmarshal([]byte(folders), &a.Folders)
	if a.Folders == nil {
		a.Folders = map[string]Picked{}
	}
	if sa, ok := m.cat.Get(a.StoreID); ok {
		a.NoGUI, a.URLSuffix = sa.NoGUI, sa.URLSuffix
	}
	return a, nil
}

const appCols = `id,store_id,catalog,name,port,state,settings,folders,COALESCE(installed_by,0),created`

func (m *Manager) Get(id string) (*App, error) {
	a, err := m.scan(m.c.DB.QueryRow(`SELECT `+appCols+` FROM apps WHERE id=?`, id))
	if err != nil {
		return nil, errors.New("App not found.")
	}
	return a, nil
}

func (m *Manager) List() ([]*App, error) {
	rows, err := m.c.DB.Query(`SELECT ` + appCols + ` FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*App
	for rows.Next() {
		a, err := m.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (m *Manager) setState(id, state string) {
	_, _ = m.c.DB.Exec(`UPDATE apps SET state=? WHERE id=?`, state, id)
}

func (m *Manager) saveSettings(a *App) {
	s, _ := json.Marshal(a.Settings)
	f, _ := json.Marshal(a.Folders)
	_, _ = m.c.DB.Exec(`UPDATE apps SET settings=?, folders=? WHERE id=?`, string(s), string(f), a.ID)
}

// UsesNAS reports the NAS roots an app depends on.
func (a *App) NASRoots() []string {
	var out []string
	for _, p := range a.Folders {
		if strings.HasPrefix(p.Root, "nas:") {
			out = append(out, p.Root)
		}
	}
	return out
}

// URL is where the app's page is, as seen from the server itself.
func (a *App) LocalURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", a.Port, a.URLSuffix)
}

// --- install ---------------------------------------------------------------

type InstallRequest struct {
	StoreID string            `json:"storeId"`
	Env     map[string]string `json:"env"`
	Folders map[string]Picked `json:"folders"`
	GPU     bool              `json:"gpu"`
}

var idRe = regexp.MustCompile(`[^a-z0-9-]+`)

// Install validates the answers, saves the app and starts installing it in the background.
func (m *Manager) Install(req InstallRequest, userID int64) (*Job, error) {
	sa, ok := m.cat.Get(req.StoreID)
	if !ok {
		return nil, errors.New("That app isn't in the store.")
	}
	env := map[string]string{}
	for _, f := range sa.Form {
		v := strings.TrimSpace(req.Env[f.EnvVariable])
		if f.Type == "random" {
			if v == "" {
				n := 32
				if f.Min != nil && *f.Min > 0 {
					n = *f.Min
				}
				v = core.RandomString(n)
			}
		} else if v == "" && f.Default != nil {
			v = fmt.Sprint(f.Default)
		}
		if v == "" && f.Required {
			return nil, fmt.Errorf("Please fill in \"%s\".", f.Label)
		}
		if v != "" && f.Min != nil && len(v) < *f.Min && f.Type != "number" && f.Type != "boolean" {
			return nil, fmt.Errorf("\"%s\" needs at least %d characters.", f.Label, *f.Min)
		}
		if f.Max != nil && len(v) > *f.Max && f.Type != "number" && f.Type != "boolean" {
			return nil, fmt.Errorf("\"%s\" can be at most %d characters.", f.Label, *f.Max)
		}
		env[f.EnvVariable] = v
	}
	folders := map[string]Picked{}
	for _, need := range sa.Folders {
		p, ok := req.Folders[need.Key]
		if !ok || p.Root == "" {
			p = Picked{Root: "local", Path: "/" + sa.ID}
		}
		if _, err := m.st.Resolve(p.Root, p.Path); err != nil {
			return nil, err
		}
		if !m.st.RootConnected(p.Root) {
			return nil, errors.New("That NAS share isn't connected right now.")
		}
		folders[need.Key] = p
	}
	id := idRe.ReplaceAllString(strings.ToLower(sa.ID), "-")
	if _, err := m.Get(id); err == nil {
		for i := 2; ; i++ {
			if _, err := m.Get(fmt.Sprintf("%s-%d", id, i)); err != nil {
				id = fmt.Sprintf("%s-%d", id, i)
				break
			}
		}
	}
	port, err := m.freePort()
	if err != nil {
		return nil, err
	}
	settings, _ := json.Marshal(AppSettings{Env: env, GPU: req.GPU, Desired: "running"})
	fj, _ := json.Marshal(folders)
	if _, err := m.c.DB.Exec(`INSERT INTO apps(id,store_id,catalog,name,port,state,settings,folders,installed_by,created) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, sa.ID, sa.Catalog, sa.Name, port, "installing", string(settings), string(fj), userID, time.Now().Unix()); err != nil {
		return nil, err
	}
	job := &Job{ID: core.RandomHex(8), AppID: id}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()
	go m.runInstall(job, id)
	return job, nil
}

func (m *Manager) freePort() (int, error) {
	used := map[int]bool{}
	rows, err := m.c.DB.Query(`SELECT port FROM apps`)
	if err == nil {
		for rows.Next() {
			var p int
			_ = rows.Scan(&p)
			used[p] = true
		}
		rows.Close()
	}
	for p := 20000; p < 30000; p++ {
		if used[p] {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		l.Close()
		return p, nil
	}
	return 0, errors.New("No free port found.")
}

func (m *Manager) runInstall(job *Job, id string) {
	fail := func(err error) {
		job.log("✗ " + err.Error())
		job.mu.Lock()
		job.Error, job.Done = err.Error(), true
		job.mu.Unlock()
		m.setState(id, "failed")
		m.c.Log("system", "install failed", id, id, false, err.Error())
	}
	job.step("Preparing")
	// a fresh install never inherits an older copy left in the app's folder
	_ = os.RemoveAll(m.recipeDir(id))
	_ = os.RemoveAll(m.undoDir(id))
	if err := m.writeCompose(id, true); err != nil {
		fail(err)
		return
	}
	job.step("Downloading")
	if err := m.compose(id, job, "pull", "--ignore-buildable"); err != nil {
		// older compose versions don't know --ignore-buildable
		if err2 := m.compose(id, job, "pull"); err2 != nil {
			// Offline, or the download site says "too many downloads": carry on
			// when every part of the app is on this computer already.
			if !m.imagesPresent(id) {
				fail(errors.New("Couldn't download the app. Check that this computer is online."))
				return
			}
			job.log("Couldn't check for a newer download; using the copy already on this computer.")
		}
	}
	job.step("Starting")
	if err := m.compose(id, job, "up", "-d", "--remove-orphans"); err != nil {
		fail(errors.New("The app didn't start. See the details below."))
		return
	}
	a, _ := m.Get(id)
	if a != nil && !a.NoGUI {
		job.step("Waiting for it to answer")
		if !waitHTTP(a.LocalURL(), 4*time.Minute) {
			job.log("It's still starting up; it will appear on the desktop when ready.")
		}
	}
	if m.hasHook(id, "setup") {
		job.step("Setting it up")
		if err := m.hook(id, "setup", job); err != nil {
			fail(errors.New("Setting the app up didn't finish. See the details below."))
			return
		}
	}
	m.setState(id, "running")
	job.step("Done")
	job.mu.Lock()
	job.Done = true
	job.mu.Unlock()
}

// writeCompose (re)writes docker-compose.yml and .env for an installed app.
func (m *Manager) writeCompose(id string, firstTime bool) error {
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	// The app runs from its own copy of the catalog entry, so it stays on its
	// version until it is updated.
	if err := m.ensureRecipe(a); err != nil {
		return err
	}
	plan, err := loadCompose(m.recipeDir(id))
	if err != nil {
		return err
	}
	dir := m.appDir(id)
	dataDir := filepath.Join(dir, "data")
	shared := filepath.Join(m.c.Cfg.DataDir, "shared")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	_ = os.MkdirAll(shared, 0o755)
	in := buildInput{AppID: id, DataDir: dataDir, SharedDir: shared, Port: a.Port, Folders: map[string]string{}, UsesNAS: len(a.NASRoots()) > 0, GPU: a.Settings.GPU, RenderGID: renderGID()}
	for key, p := range a.Folders {
		real, err := m.st.Resolve(p.Root, p.Path)
		if err != nil {
			return err
		}
		if m.st.RootConnected(p.Root) {
			if err := os.MkdirAll(real, 0o775); err == nil {
				_ = os.Chown(real, storage.AppUID, storage.AppGID)
			}
		}
		in.Folders[key] = real
	}
	// Starting files that come with the catalog app (Runtipi's "data" folder).
	if firstTime {
		if sa, ok := m.cat.Get(a.StoreID); ok {
			if src := filepath.Join(sa.dir, "data"); dirExists(src) {
				_ = copyTree(src, dataDir)
			}
		}
	}
	for _, d := range plan.dirsToCreate(in) {
		if err := os.MkdirAll(d, 0o755); err == nil && firstTime {
			_ = os.Chown(d, storage.AppUID, storage.AppGID)
		}
	}
	if firstTime {
		_ = chownTree(dataDir, storage.AppUID, storage.AppGID)
	}
	yml, err := plan.Build(in)
	if err != nil {
		return err
	}
	header := "# Written by ZeroOS. Changes here are replaced when the app's settings change.\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), append([]byte(header), yml...), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ".env"), []byte(m.envFile(a, dataDir, shared)), 0o600)
}

func (m *Manager) envFile(a *App, dataDir, shared string) string {
	ip := hostIP()
	vals := map[string]string{
		"TZ": hostTZ(), "APP_ID": a.ID, "APP_PORT": strconv.Itoa(a.Port), "APP_DATA_DIR": dataDir,
		"ROOT_FOLDER_HOST": shared, "APP_DOMAIN": fmt.Sprintf("%s:%d", ip, a.Port), "LOCAL_DOMAIN": "local",
		"APP_PROTOCOL": "http", "APP_EXPOSED": "false", "APP_HOST": ip, "INTERNAL_IP": ip,
		"NETWORK_INTERFACE": defaultIface(), "TIPI_UID": "1000", "TIPI_GID": "1000", "UID": "1000", "GID": "1000",
		"PUID": "1000", "PGID": "1000", "TIPI_VERSION": "4.5.0", "HOST_NAME": hostName(),
	}
	for k, v := range a.Settings.Env {
		vals[k] = v
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		// single quotes: compose takes the value literally, so passwords with $ are safe
		fmt.Fprintf(&b, "%s='%s'\n", k, strings.ReplaceAll(vals[k], "'", ""))
	}
	return b.String()
}

// imagesPresent tells whether Docker already has every image the app uses.
func (m *Manager) imagesPresent(id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "compose", "-p", project(id), "--project-directory", m.appDir(id),
		"-f", filepath.Join(m.appDir(id), "docker-compose.yml"), "config", "--images").Output()
	if err != nil {
		return false
	}
	images := strings.Fields(string(out))
	if len(images) == 0 {
		return false
	}
	for _, img := range images {
		if exec.CommandContext(ctx, "docker", "image", "inspect", img).Run() != nil {
			return false
		}
	}
	return true
}

// compose runs docker compose for one app, streaming output into the job.
func (m *Manager) compose(id string, job *Job, args ...string) error {
	full := append([]string{"compose", "-p", project(id), "--project-directory", m.appDir(id), "-f", filepath.Join(m.appDir(id), "docker-compose.yml")}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", full...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" && job != nil {
				job.log(line)
			}
		}
		close(done)
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	return err
}

func waitHTTP(url string, max time.Duration) bool {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	end := time.Now().Add(max)
	for time.Now().Before(end) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return true
			}
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// --- lifecycle -------------------------------------------------------------

func (m *Manager) lock(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[id] {
		return false
	}
	m.busy[id] = true
	return true
}

func (m *Manager) unlock(id string) {
	m.mu.Lock()
	delete(m.busy, id)
	m.mu.Unlock()
}

var ErrBusy = errors.New("The app is busy with something else. Try again in a moment.")

func (m *Manager) Start(id string) error {
	if !m.lock(id) {
		return ErrBusy
	}
	defer m.unlock(id)
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	a.Settings.Desired = "running"
	m.saveSettings(a)
	for _, r := range a.NASRoots() {
		if !m.st.RootConnected(r) {
			m.setState(id, "waiting")
			return nil
		}
	}
	if err := m.writeCompose(id, false); err != nil {
		return err
	}
	if err := m.compose(id, nil, "up", "-d", "--remove-orphans"); err != nil {
		return errors.New("The app didn't start. Check its log.")
	}
	m.setState(id, "running")
	return nil
}

func (m *Manager) Stop(id string) error {
	if !m.lock(id) {
		return ErrBusy
	}
	defer m.unlock(id)
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	a.Settings.Desired = "stopped"
	m.saveSettings(a)
	if err := m.compose(id, nil, "stop"); err != nil {
		return errors.New("Couldn't stop the app.")
	}
	_ = m.hook(id, "stop", nil)
	m.setState(id, "stopped")
	return nil
}

func (m *Manager) Restart(id string) error {
	if err := m.Stop(id); err != nil {
		return err
	}
	return m.Start(id)
}

// SetGPU turns the graphics-chip option on or off and recreates the app.
func (m *Manager) SetGPU(id string, on bool) error {
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	if on && !dirExists("/dev/dri") {
		return errors.New("This computer has no graphics chip that apps can use.")
	}
	a.Settings.GPU = on
	m.saveSettings(a)
	if a.Settings.Desired == "running" {
		return m.Start(id)
	}
	return m.writeCompose(id, false)
}

// SetFolder changes one picked folder and recreates the app.
func (m *Manager) SetFolder(id, key string, p Picked) error {
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	if _, ok := a.Folders[key]; !ok {
		return errors.New("This app doesn't have that folder.")
	}
	if _, err := m.st.Resolve(p.Root, p.Path); err != nil {
		return err
	}
	a.Folders[key] = p
	m.saveSettings(a)
	if a.Settings.Desired == "running" {
		return m.Start(id)
	}
	return m.writeCompose(id, false)
}

// Remove stops the app and deletes it; its own data is deleted too when asked.
// Folders on the NAS are never touched.
func (m *Manager) Remove(id string, deleteData bool) error {
	if !m.lock(id) {
		return ErrBusy
	}
	defer m.unlock(id)
	if _, err := m.Get(id); err != nil {
		return err
	}
	m.setState(id, "removing")
	_ = m.hook(id, "remove", nil)
	args := []string{"down", "--remove-orphans"}
	if deleteData {
		args = append(args, "--volumes")
	}
	if _, err := os.Stat(filepath.Join(m.appDir(id), "docker-compose.yml")); err == nil {
		if err := m.compose(id, nil, args...); err != nil {
			m.setState(id, "failed")
			return errors.New("Couldn't remove the app's containers.")
		}
	}
	if deleteData {
		_ = os.RemoveAll(m.appDir(id))
	}
	_, err := m.c.DB.Exec(`DELETE FROM apps WHERE id=?`, id)
	return err
}

// Logs returns the last lines of the app's output.
func (m *Manager) Logs(id string, lines int) (string, error) {
	if _, err := m.Get(id); err != nil {
		return "", err
	}
	full := []string{"compose", "-p", project(id), "--project-directory", m.appDir(id), "-f", filepath.Join(m.appDir(id), "docker-compose.yml"), "logs", "--no-color", "--tail", strconv.Itoa(lines)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", full...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", errors.New("Couldn't read the app's log.")
	}
	return string(out), nil
}

// --- live status -----------------------------------------------------------

type container struct {
	Labels string `json:"Labels"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Name   string `json:"Names"`
}

// liveStates asks Docker for every ZeroOS container in one call.
func liveStates() map[string][]container {
	out := map[string][]container{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label=zeroos.app", "--format", "{{json .}}").Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var c container
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		for _, l := range strings.Split(c.Labels, ",") {
			if strings.HasPrefix(l, "zeroos.app=") {
				id := strings.TrimPrefix(l, "zeroos.app=")
				out[id] = append(out[id], c)
			}
		}
	}
	return out
}

// Status fills in each app's live state.
func (m *Manager) Status(list []*App) {
	live := liveStates()
	for _, a := range list {
		switch a.State {
		case "installing", "removing", "failed", "waiting", "updating":
			if a.State == "waiting" {
				a.Problem = "Waiting for the NAS to come back. It starts by itself."
			}
			continue
		}
		if live == nil {
			continue
		}
		cs := live[a.ID]
		if len(cs) == 0 {
			a.State = "stopped"
			if a.Settings.Desired == "running" {
				a.State = "crashed"
				a.Problem = "The app isn't running. Try starting it."
			}
			continue
		}
		running, starting, exited := 0, 0, 0
		for _, c := range cs {
			switch {
			case c.State == "running" && strings.Contains(c.Status, "health: starting"):
				starting++
			case c.State == "running" && strings.Contains(c.Status, "unhealthy"):
				running++
			case c.State == "running":
				running++
			case c.State == "restarting":
				exited++
			default:
				exited++
			}
		}
		switch {
		case exited > 0 && a.Settings.Desired == "running":
			a.State = "crashed"
			a.Problem = "Part of the app stopped unexpectedly. Check its log or restart it."
		case exited > 0 && running == 0:
			a.State = "stopped"
		case starting > 0:
			a.State = "starting"
		default:
			a.State = "running"
		}
	}
}

// Reconcile keeps apps that use the NAS in step with the NAS: stop them when a
// share disappears, start them when it comes back, and restart them if they stop.
func (m *Manager) Reconcile() {
	list, err := m.List()
	if err != nil {
		return
	}
	live := liveStates()
	for _, a := range list {
		roots := a.NASRoots()
		if len(roots) == 0 || a.Settings.Desired != "running" || a.State == "installing" || a.State == "removing" || a.State == "updating" {
			continue
		}
		ok := true
		for _, r := range roots {
			if !m.st.RootConnected(r) {
				ok = false
			}
		}
		isRunning := false
		for _, c := range live[a.ID] {
			if c.State == "running" {
				isRunning = true
			}
		}
		switch {
		case !ok && a.State != "waiting":
			if m.lock(a.ID) {
				_ = m.compose(a.ID, nil, "stop")
				m.setState(a.ID, "waiting")
				m.unlock(a.ID)
				m.c.Log("system", "paused (NAS offline)", a.Name, a.ID, true, "")
			}
		case ok && (a.State == "waiting" || !isRunning):
			if m.lock(a.ID) {
				if m.writeCompose(a.ID, false) == nil && m.compose(a.ID, nil, "up", "-d") == nil {
					if a.State == "waiting" {
						m.c.Log("system", "started (NAS back)", a.Name, a.ID, true, "")
					}
					m.setState(a.ID, "running")
				}
				m.unlock(a.ID)
			}
		}
	}
}

// Frameable tells whether the app's page may be shown inside a ZeroOS window.
func (m *Manager) Frameable(a *App) bool {
	m.mu.Lock()
	v, ok := m.frame[a.ID]
	m.mu.Unlock()
	if ok {
		return v
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(a.LocalURL())
	if err != nil {
		return true // can't tell yet; try the window
	}
	resp.Body.Close()
	xfo := strings.ToLower(resp.Header.Get("X-Frame-Options"))
	csp := strings.ToLower(resp.Header.Get("Content-Security-Policy"))
	ok = xfo == "" && !strings.Contains(csp, "frame-ancestors 'none'") && !strings.Contains(csp, "frame-ancestors 'self'")
	m.mu.Lock()
	m.frame[a.ID] = ok
	m.mu.Unlock()
	return ok
}

// --- small helpers ---------------------------------------------------------

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if _, err := os.Stat(target); err == nil {
			return nil // never overwrite the app's own files
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Lchown(p, uid, gid)
		}
		return nil
	})
}

func renderGID() int {
	b, err := os.ReadFile("/etc/group")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) > 2 && f[0] == "render" {
			n, _ := strconv.Atoi(f[2])
			return n
		}
	}
	return 0
}

func hostTZ() string {
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	if l, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(l, "zoneinfo/"); i >= 0 {
			return l[i+9:]
		}
	}
	return "UTC"
}

// hostName is this computer's name on the home network, where it answers as <name>.local.
func hostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	return strings.ToLower(strings.SplitN(h, ".", 2)[0])
}

// hook runs a script that comes with one of ZeroOS's own apps, with the app's
// settings in its environment and its output in the install log.
func (m *Manager) hook(id, name string, job *Job) error {
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	sa, _ := m.cat.Get(a.StoreID)
	script := m.cat.Hook(sa, name)
	if script == nil {
		return nil
	}
	dir := m.appDir(id)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-s")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Env = append(os.Environ(), "ZEROOS_APP_ID="+id, "ZEROOS_PROJECT="+project(id), "ZEROOS_APP_DIR="+dir,
		"APP_DATA_DIR="+filepath.Join(dir, "data"), "APP_PORT="+strconv.Itoa(a.Port), "HOST_NAME="+hostName(), "HOST_IP="+hostIP(), "TZ="+hostTZ())
	for k, v := range a.Settings.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" && job != nil {
				job.log(line)
			}
		}
		close(done)
	}()
	err = cmd.Run()
	pw.Close()
	<-done
	return err
}

// hasHook tells whether an installed app comes with that script.
func (m *Manager) hasHook(id, name string) bool {
	a, err := m.Get(id)
	if err != nil {
		return false
	}
	sa, _ := m.cat.Get(a.StoreID)
	return m.cat.Hook(sa, name) != nil
}

// hostIP is this computer's address on the home network.
func hostIP() string {
	conn, err := net.Dial("udp", "192.168.0.1:80")
	if err == nil {
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "127.0.0.1"
}

func defaultIface() string {
	ip := hostIP()
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if strings.HasPrefix(a.String(), ip+"/") {
				return ifc.Name
			}
		}
	}
	return "eth0"
}

// HostIP is exported for the server.
func HostIP() string { return hostIP() }
