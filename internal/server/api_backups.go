package server

import (
	"net/http"
	"sort"

	"github.com/MightyX3N/zeroos/internal/apps"
)

// --- backups ---------------------------------------------------------------

// backupsInfo is the Backups page in Settings: the plan, every app with its
// newest backup, and the backups of apps that aren't installed any more.
func (s *Server) backupsInfo(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID      string `json:"id"`
		StoreID string `json:"storeId"`
		Name    string `json:"name"`
		Auto    bool   `json:"auto"`
		Count   int    `json:"count"`
		Size    int64  `json:"size"`
		Last    int64  `json:"last"`
	}
	type gone struct {
		ID      string        `json:"id"`
		StoreID string        `json:"storeId"`
		Name    string        `json:"name"`
		Backups []apps.Backup `json:"backups"`
	}
	all := s.Apps.AllBackups()
	rows, orphans := []row{}, []gone{}
	for _, a := range s.appList() {
		rw := row{ID: a.ID, StoreID: a.StoreID, Name: a.Name, Auto: !a.Settings.NoBackup}
		for _, b := range all[a.ID] {
			rw.Count++
			rw.Size += b.Size
			if b.At > rw.Last {
				rw.Last = b.At
			}
		}
		delete(all, a.ID)
		rows = append(rows, rw)
	}
	for id, list := range all {
		for i := range list {
			list[i].Env = nil // answers given at install can hold passwords
		}
		orphans = append(orphans, gone{ID: id, StoreID: list[0].StoreID, Name: list[0].Name, Backups: list})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Name < orphans[j].Name })
	ok(w, map[string]any{"plan": s.Apps.Plan(), "apps": rows, "gone": orphans, "canInstall": userOf(r).Can("apps.install")})
}

func (s *Server) backupsPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Root string `json:"root"`
		Path string `json:"path"`
		When string `json:"when"`
		Hour int    `json:"hour"`
		Keep int    `json:"keep"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	err := s.Apps.SetPlan(in.Root, in.Path, in.When, in.Hour, in.Keep)
	s.log(r, "changed the backup settings", "", "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, s.Apps.Plan())
}

// backupsRun backs everything up now, in the background.
func (s *Server) backupsRun(w http.ResponseWriter, r *http.Request) {
	p := s.Apps.Plan()
	if !p.Set || !p.Connected {
		fail(w, 400, "The place for backups isn't set or isn't connected.")
		return
	}
	if p.Running {
		fail(w, 400, "Backups are being made already.")
		return
	}
	s.log(r, "started a backup of every app", "", "", true, "")
	go func() { _, _, _ = s.Apps.BackUpAll(false) }()
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) backupsPutBack(w http.ResponseWriter, r *http.Request) {
	if !userOf(r).Can("apps.install") {
		fail(w, 403, "You don't have permission to do that.")
		return
	}
	job, err := s.Apps.InstallBackup(r.PathValue("app"), r.PathValue("file"), userOf(r).ID)
	s.log(r, "started bringing back from a backup", r.PathValue("app"), "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]string{"job": job.ID, "appId": job.AppID})
}

func (s *Server) backupsDelete(w http.ResponseWriter, r *http.Request) {
	err := s.Apps.DeleteBackup(r.PathValue("app"), r.PathValue("file"))
	s.log(r, "deleted a backup of", r.PathValue("app"), "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

// appBackups lists one app's backups.
func (s *Server) appBackups(w http.ResponseWriter, r *http.Request) {
	a, _ := s.Apps.Get(r.PathValue("id"))
	list := s.Apps.Backups(a.ID)
	for i := range list {
		list[i].Env = nil
	}
	ok(w, map[string]any{"plan": s.Apps.Plan(), "auto": !a.Settings.NoBackup, "list": list, "canManage": userOf(r).Can("backups.manage")})
}

func (s *Server) appBackupMake(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	job, err := s.Apps.MakeBackup(id, false)
	s.log(r, "started a backup of", a.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]string{"job": job.ID, "appId": id})
}

func (s *Server) appBackupRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	job, err := s.Apps.RestoreBackup(id, r.PathValue("file"))
	s.log(r, "started putting a backup back into", a.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]string{"job": job.ID, "appId": id})
}

func (s *Server) appBackupDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	err := s.Apps.DeleteBackup(id, r.PathValue("file"))
	s.log(r, "deleted a backup of", a.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) appBackupAuto(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	if err := s.Apps.SetAutoBackup(r.PathValue("id"), in.On); err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}
