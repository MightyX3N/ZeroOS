package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
)

// Updating apps, with a way back.
//
// An installed app keeps its own copy of the catalog entry it was installed
// from (its "recipe"), so it stays on that version until someone updates it,
// however often the store catalog refreshes.
//
// An update first stops the app and copies its own data aside, together with
// the old recipe. If the new version doesn't start, that copy is put back
// right away. Otherwise it stays for a week, so the update can be undone.
// Folders on the NAS are never copied or changed.

// UndoDays is how long the copy for undoing an update is kept.
const UndoDays = 7

// UndoPoint describes the copy kept for undoing the last update.
type UndoPoint struct {
	Version string `json:"version"` // the version it goes back to
	At      int64  `json:"at"`      // when the update was made
	Size    int64  `json:"size"`    // bytes the copy takes
	Build   bool   `json:"build"`   // the update was a newer build of the same version
	Edit    bool   `json:"edit"`    // the change was an edited compose file
	Restore bool   `json:"restore"` // the change was a backup that was put back
}

// appBefore is what an edit can change besides the recipe, kept for putting it back.
type appBefore struct {
	Name    string            `json:"name"`
	Port    int               `json:"port"`
	Folders map[string]Picked `json:"folders"`
	Env     map[string]string `json:"env"`
}

// UpdateInfo says which newer version the store has for an installed app.
type UpdateInfo struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Build bool   `json:"build"` // same version, but a newer build of it can be downloaded
}

var recipeFiles = []string{"config.json", "docker-compose.json", "docker-compose.yml"}

func (m *Manager) recipeDir(id string) string { return filepath.Join(m.appDir(id), "recipe") }
func (m *Manager) undoDir(id string) string   { return filepath.Join(m.appDir(id), "undo") }

// copyRecipe copies the files that say how an app runs.
func copyRecipe(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	n := 0
	for _, f := range recipeFiles {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			_ = os.Remove(filepath.Join(dst, f))
			continue
		}
		if err := os.WriteFile(filepath.Join(dst, f), b, 0o644); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("the app's catalog entry is empty")
	}
	return nil
}

// recipeVersion reads the version out of a recipe folder.
func recipeVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return ""
	}
	var rc runtipiConfig
	_ = json.Unmarshal(b, &rc)
	return rc.Version
}

// sameRecipe tells whether two recipe folders run the app the same way.
func sameRecipe(a, b string) bool {
	if recipeVersion(a) != recipeVersion(b) {
		return false
	}
	for _, f := range recipeFiles[1:] {
		x, _ := os.ReadFile(filepath.Join(a, f))
		y, _ := os.ReadFile(filepath.Join(b, f))
		if !bytes.Equal(bytes.TrimSpace(x), bytes.TrimSpace(y)) {
			return false
		}
	}
	return true
}

// ensureRecipe gives an app its own recipe if it has none yet: at install, and
// once for apps installed before recipes existed.
func (m *Manager) ensureRecipe(a *App) error {
	dir := m.recipeDir(a.ID)
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		return nil
	}
	sa, ok := m.cat.Get(a.StoreID)
	if !ok {
		return errors.New("This app is no longer in the store catalog.")
	}
	if err := copyRecipe(sa.dir, dir); err != nil {
		return err
	}
	a.Settings.Version = recipeVersion(dir)
	m.saveSettings(a)
	return nil
}

// UpdateFor says whether the store has a newer version of an installed app.
func (m *Manager) UpdateFor(a *App) *UpdateInfo {
	dir := m.recipeDir(a.ID)
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		return nil // not pinned yet; it is on first start
	}
	sa, ok := m.cat.Get(a.StoreID)
	if !ok {
		// gone from the store: only a newer build of what it runs can come
		if m.newerBuild(a.ID) {
			v := recipeVersion(dir)
			return &UpdateInfo{From: v, To: v, Build: true}
		}
		return nil
	}
	if sameRecipe(dir, sa.dir) {
		if m.newerBuild(a.ID) {
			v := recipeVersion(dir)
			return &UpdateInfo{From: v, To: v, Build: true}
		}
		return nil
	}
	from, to := recipeVersion(dir), sa.Version
	if from == to {
		// same version number, but it runs differently (a fix in the catalog)
		to += " (revised)"
	}
	return &UpdateInfo{From: from, To: to}
}

// UndoFor returns the copy kept for undoing the last update, if there is one.
func (m *Manager) UndoFor(a *App) *UndoPoint {
	if a.Settings.Undo == nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(m.undoDir(a.ID), "data")); err != nil {
		return nil
	}
	return a.Settings.Undo
}

// ErrNoSpace means there isn't room for the copy an update makes first.
type ErrNoSpace struct{ Need, Free int64 }

func (e *ErrNoSpace) Error() string {
	return fmt.Sprintf("There isn't enough free space to keep a copy for undoing this update (it needs %s, %s is free).", humanBytes(e.Need), humanBytes(e.Free))
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

var freeSpace = func(dir string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// copyData copies an app's own data, keeping owners and permissions.
// On disks that can, the copy shares its blocks with the original (instant, no extra space).
func copyData(src, dst string) error {
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	out, err := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", src, dst).CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dst)
		return fmt.Errorf("couldn't copy the app's data: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) newJob(id string) *Job {
	job := &Job{ID: core.RandomHex(8), AppID: id}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()
	return job
}

func (job *Job) finish(err error) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if err != nil {
		job.Error = err.Error()
	}
	job.Done = true
}

// Update moves an installed app to the version in the store. Unless noUndo is
// set, it keeps a copy of the app's data to go back to.
func (m *Manager) Update(id string, noUndo bool) (*Job, error) {
	a, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if m.UpdateFor(a) == nil {
		return nil, errors.New("This app is already on the newest version.")
	}
	dataDir := filepath.Join(m.appDir(id), "data")
	if !noUndo {
		need := dirSize(dataDir)
		need += need/10 + 200<<20
		if free := freeSpace(m.appDir(id)); free < need {
			return nil, &ErrNoSpace{Need: need, Free: free}
		}
	}
	if !m.lock(id) {
		return nil, ErrBusy
	}
	job := m.newJob(id)
	go func() {
		defer m.unlock(id)
		m.runUpdate(job, id, noUndo, nil)
	}()
	return job, nil
}

// runUpdate replaces what an app runs: with the store's newer version, a newer
// build of the same one, or (change) an edited compose file of a custom app.
func (m *Manager) runUpdate(job *Job, id string, noUndo bool, change *recipeChange) {
	a, err := m.Get(id)
	if err != nil {
		job.finish(err)
		return
	}
	up := m.UpdateFor(a)
	if change != nil {
		up = &UpdateInfo{From: a.Settings.Version, To: a.Settings.Version}
	}
	if up == nil {
		job.finish(errors.New("This app is already on the newest version."))
		return
	}
	sa, _ := m.cat.Get(a.StoreID)
	wasState, old := a.State, a.Settings.Version
	dataDir, recipe, undo := filepath.Join(m.appDir(id), "data"), m.recipeDir(id), m.undoDir(id)
	m.setState(id, "updating")
	back := func(state string) { m.setState(id, state) }

	job.step("Stopping it")
	if err := m.compose(id, job, "stop"); err != nil {
		back(wasState)
		job.finish(errors.New("Couldn't stop the app, so nothing was changed."))
		return
	}
	_ = m.hook(id, "stop", nil)

	_ = os.RemoveAll(undo)
	a.Settings.Undo = nil
	if !noUndo {
		job.step("Keeping a copy to go back to")
		err := copyData(dataDir, filepath.Join(undo, "data"))
		if err == nil {
			err = copyRecipe(recipe, filepath.Join(undo, "recipe"))
		}
		if err != nil {
			_ = os.RemoveAll(undo)
			job.log(err.Error())
			m.startAgain(id, job, wasState)
			job.finish(errors.New("Couldn't keep a copy to go back to, so the app was not updated."))
			return
		}
		// the images it runs now keep a name of their own, so the download can't push them out
		m.keepImages(id)
		// and the app's name, page port, folders and answers, which an edit can change
		was, _ := json.Marshal(appBefore{Name: a.Name, Port: a.Port, Folders: a.Folders, Env: a.Settings.Env})
		_ = os.WriteFile(filepath.Join(undo, "app.json"), was, 0o600)
		a.Settings.Undo = &UndoPoint{Version: old, At: time.Now().Unix(), Size: dirSize(filepath.Join(undo, "data")), Build: up.Build, Edit: change != nil}
	}

	switch {
	case change != nil:
		if err := copyRecipe(change.dir, recipe); err != nil {
			_ = m.putBack(id, job, a, wasState, noUndo)
			job.finish(errors.New("Couldn't save the new compose file, so the app was not changed."))
			return
		}
		a.Name, a.Port, a.Folders, a.Settings.Env = change.name, change.port, change.folders, change.env
		_, _ = m.c.DB.Exec(`UPDATE apps SET name=?, port=? WHERE id=?`, a.Name, a.Port, id)
	case !up.Build:
		if err := copyRecipe(sa.dir, recipe); err != nil {
			_ = m.putBack(id, job, a, wasState, noUndo)
			job.finish(errors.New("Couldn't read the new version from the store, so the app was not updated."))
			return
		}
	}
	a.Settings.Version = recipeVersion(recipe)
	m.saveSettings(a)

	fail := func(why string) {
		job.log("✗ " + why)
		if noUndo {
			back("failed")
			m.c.Log("system", "update failed", a.Name, id, false, why)
			job.finish(errors.New(why + " There is no copy to go back to."))
			return
		}
		if err := m.putBack(id, job, a, wasState, false); err != nil {
			back("failed")
			m.c.Log("system", "update failed", a.Name, id, false, why+" Putting the previous version back failed too.")
			job.finish(errors.New(why + " Putting the previous version back didn't work either: " + err.Error()))
			return
		}
		m.c.Log("system", "update failed, previous version put back", a.Name, id, false, why)
		what := "The previous version (" + old + ")"
		if up.Build {
			what = "The previous build"
		}
		if change != nil {
			what = "Your previous compose file"
		}
		job.finish(errors.New(why + " " + what + " was put back, with everything as it was."))
	}

	if err := m.writeCompose(id, false); err != nil {
		fail("The new version couldn't be prepared.")
		return
	}
	job.step("Downloading the new version")
	if err := m.compose(id, job, "pull", "--ignore-buildable"); err != nil {
		if err2 := m.compose(id, job, "pull"); err2 != nil && !m.imagesPresent(id) {
			fail("The new version couldn't be downloaded.")
			return
		}
	}
	job.step("Starting it")
	if err := m.compose(id, job, "up", "-d", "--remove-orphans"); err != nil {
		fail("The new version didn't start.")
		return
	}
	job.step("Checking that it works")
	if !m.comesUp(a) {
		fail("The new version didn't start properly.")
		return
	}
	if wasState == "stopped" {
		_ = m.compose(id, nil, "stop")
		_ = m.hook(id, "stop", nil)
		back("stopped")
	} else {
		back("running")
	}
	m.mu.Lock()
	delete(m.frame, id)
	m.newer[id] = false
	m.mu.Unlock()
	detail := fmt.Sprintf("%s (%s to %s)", a.Name, old, a.Settings.Version)
	if up.Build {
		detail = a.Name + " (a newer build)"
	}
	if change != nil {
		m.c.Log("system", "changed the compose file of", a.Name, id, true, "")
	} else {
		m.c.Log("system", "updated", detail, id, true, "")
	}
	job.step("Done")
	job.finish(nil)
}

// brokenParts counts the app's containers that crashed or keep restarting.
// A part that finished its job cleanly (some apps have a one-time helper) is fine.
func brokenParts(id string) (broken, total int) {
	for _, c := range liveStates()[id] {
		total++
		switch {
		case c.State == "running":
		case c.State == "exited" && strings.HasPrefix(c.Status, "Exited (0)"):
		default:
			broken++
		}
	}
	return
}

// comesUp waits for a freshly started app to work: its page answers and none
// of its parts crashes. It gives up early when a part keeps crashing, rather
// than waiting the whole time for a page that will never come.
func (m *Manager) comesUp(a *App) bool {
	client := probeClient(5 * time.Second)
	answers := func() bool {
		if a.NoGUI {
			return true
		}
		resp, err := client.Get(a.LocalURL())
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode < 500
	}
	crashes, good := 0, 0
	for end := time.Now().Add(4 * time.Minute); time.Now().Before(end); time.Sleep(3 * time.Second) {
		broken, total := brokenParts(a.ID)
		if total == 0 || broken > 0 {
			crashes++
			good = 0
			if crashes >= 4 { // seen broken four times: it isn't coming up
				return false
			}
			continue
		}
		if !answers() {
			good = 0
			continue
		}
		// it answers and everything runs: make sure it stays that way for a moment
		if good++; good >= 3 {
			return true
		}
	}
	return false
}

// startAgain starts an app as it is, after an update that changed nothing.
func (m *Manager) startAgain(id string, job *Job, wasState string) {
	if wasState == "stopped" {
		m.setState(id, "stopped")
		return
	}
	if err := m.compose(id, job, "up", "-d", "--remove-orphans"); err != nil {
		m.setState(id, "crashed")
		return
	}
	m.setState(id, "running")
}

// putBack restores the copy made before an update: data and recipe.
func (m *Manager) putBack(id string, job *Job, a *App, wasState string, noUndo bool) error {
	if job != nil {
		job.step("Putting the previous version back")
	}
	_ = m.compose(id, job, "down", "--remove-orphans")
	_ = m.hook(id, "stop", nil)
	dataDir, recipe, undo := filepath.Join(m.appDir(id), "data"), m.recipeDir(id), m.undoDir(id)
	if !noUndo {
		if _, err := os.Stat(filepath.Join(undo, "data")); err != nil {
			return errors.New("the copy is gone")
		}
		trash := dataDir + ".old"
		_ = os.RemoveAll(trash)
		if err := os.Rename(dataDir, trash); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(undo, "data"), dataDir); err != nil {
			_ = os.Rename(trash, dataDir)
			return err
		}
		_ = os.RemoveAll(trash)
		if err := copyRecipe(filepath.Join(undo, "recipe"), recipe); err != nil {
			return err
		}
		m.restoreImages(id)
		if b, err := os.ReadFile(filepath.Join(undo, "app.json")); err == nil {
			var was appBefore
			if json.Unmarshal(b, &was) == nil && was.Port > 0 {
				a.Name, a.Port, a.Folders, a.Settings.Env = was.Name, was.Port, was.Folders, was.Env
				_, _ = m.c.DB.Exec(`UPDATE apps SET name=?, port=? WHERE id=?`, a.Name, a.Port, id)
			}
		}
	}
	m.dropKeptImages(id)
	_ = os.RemoveAll(undo)
	a.Settings.Undo = nil
	a.Settings.Version = recipeVersion(recipe)
	m.saveSettings(a)
	if err := m.writeCompose(id, false); err != nil {
		return err
	}
	if wasState == "stopped" {
		m.setState(id, "stopped")
		return nil
	}
	if err := m.compose(id, job, "up", "-d", "--remove-orphans"); err != nil {
		return errors.New("the previous version didn't start")
	}
	m.setState(id, "running")
	m.mu.Lock()
	delete(m.frame, id)
	m.mu.Unlock()
	return nil
}

// Undo goes back to how the app was before its last update.
func (m *Manager) Undo(id string) (*Job, error) {
	a, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if m.UndoFor(a) == nil {
		return nil, errors.New("There is no update to undo any more.")
	}
	if !m.lock(id) {
		return nil, ErrBusy
	}
	job := m.newJob(id)
	go func() {
		defer m.unlock(id)
		was, from, to, restore := a.State, a.Settings.Version, a.Settings.Undo.Version, a.Settings.Undo.Restore
		m.setState(id, "updating")
		if err := m.putBack(id, job, a, was, false); err != nil {
			m.setState(id, "failed")
			m.c.Log("system", "undo of update failed", a.Name, id, false, err.Error())
			job.finish(errors.New("Going back didn't work: " + err.Error()))
			return
		}
		m.mu.Lock()
		m.newer[id] = false // asked again at the next check
		m.mu.Unlock()
		if restore {
			m.c.Log("system", "went back to before a backup was put back into", a.Name, id, true, "")
		} else {
			m.c.Log("system", "update undone", fmt.Sprintf("%s (%s back to %s)", a.Name, from, to), id, true, "")
		}
		job.step("Done")
		job.finish(nil)
	}()
	return job, nil
}

// DropUndo deletes the copy kept for undoing the last update.
func (m *Manager) DropUndo(id string) error {
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	if !m.lock(id) {
		return ErrBusy
	}
	defer m.unlock(id)
	m.dropKeptImages(id)
	_ = os.RemoveAll(m.undoDir(id))
	a.Settings.Undo = nil
	m.saveSettings(a)
	return nil
}

// ExpireUndo deletes undo copies older than UndoDays.
func (m *Manager) ExpireUndo() {
	list, err := m.List()
	if err != nil {
		return
	}
	for _, a := range list {
		if u := a.Settings.Undo; u != nil && time.Since(time.Unix(u.At, 0)) > UndoDays*24*time.Hour {
			if m.DropUndo(a.ID) == nil {
				m.c.Log("system", "deleted the copy for undoing an update (a week old)", a.Name, a.ID, true, "")
			}
		}
	}
}

// Recover tidies up after ZeroOS itself was stopped in the middle of an
// install or update, and gives apps from before recipes existed their own
// recipe, so they too stay on their version from now on.
func (m *Manager) Recover() {
	_, _ = m.c.DB.Exec(`UPDATE apps SET state='failed' WHERE state='installing'`)
	_, _ = m.c.DB.Exec(`UPDATE apps SET state='running' WHERE state IN ('updating','removing','backup')`)
	list, _ := m.List()
	for _, a := range list {
		if a.State != "failed" {
			_ = m.ensureRecipe(a)
		}
	}
}
