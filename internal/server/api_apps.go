package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/MightyX3N/zeroos/internal/apps"
	"github.com/MightyX3N/zeroos/internal/auth"
	"github.com/MightyX3N/zeroos/internal/storage"
)

// --- desktop ---------------------------------------------------------------

type deskApp struct {
	ID      string   `json:"id"`
	StoreID string   `json:"storeId"`
	Name    string   `json:"name"`
	State   string   `json:"state"`
	Problem string   `json:"problem,omitempty"`
	Perms   []string `json:"perms"`
	Shared  bool     `json:"shared"`
	NoGUI   bool     `json:"noGui"`
	Update  string   `json:"update,omitempty"` // a newer version in the store, for people who may update it
}

func (s *Server) visibleApps(u *auth.User) []*apps.App {
	list, _ := s.Apps.List()
	var out []*apps.App
	for _, a := range list {
		if s.Auth.CanApp(u, a.ID, "app.view") {
			out = append(out, a)
		}
	}
	s.Apps.Status(out)
	return out
}

func (s *Server) appList() []*apps.App {
	list, _ := s.Apps.List()
	return list
}

func (s *Server) sharedApps() map[string]bool {
	out := map[string]bool{}
	rows, err := s.C.DB.Query(`SELECT app_id FROM tunnels WHERE kind='app'`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out[id] = true
	}
	for _, a := range s.appList() {
		if s.Playit.SecureFor(a.PublicPort()) != nil {
			out[a.ID] = true
		}
	}
	return out
}

func (s *Server) desktop(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	shared := s.sharedApps()
	list := []deskApp{}
	attention := []map[string]string{}
	for _, a := range s.visibleApps(u) {
		da := deskApp{ID: a.ID, StoreID: a.StoreID, Name: a.Name, State: a.State, Problem: a.Problem, Perms: s.Auth.AppPermsFor(u, a.ID), Shared: shared[a.ID], NoGUI: a.NoGUI}
		if up := s.Apps.UpdateFor(a); up != nil && s.Auth.CanApp(u, a.ID, "app.update") {
			da.Update = up.To
			if up.Build {
				da.Update = "newer build"
			}
		}
		list = append(list, da)
		switch a.State {
		case "waiting":
			attention = append(attention, map[string]string{"appId": a.ID, "storeId": a.StoreID, "name": a.Name, "text": a.Name + " is waiting for your NAS. It starts by itself when the NAS is back."})
		case "crashed":
			attention = append(attention, map[string]string{"appId": a.ID, "storeId": a.StoreID, "name": a.Name, "text": a.Name + " stopped unexpectedly. Try restarting it."})
		case "failed":
			attention = append(attention, map[string]string{"appId": a.ID, "storeId": a.StoreID, "name": a.Name, "text": a.Name + " didn't install. Open it to see what went wrong."})
		}
	}
	if u.Can("storage.manage") {
		shares, _ := s.Store.Shares()
		for _, sh := range shares {
			if !sh.Connected {
				attention = append(attention, map[string]string{"share": sh.Name, "text": "The NAS share " + sh.Name + " isn't connected. ZeroOS keeps trying."})
			}
		}
	}
	if u.Can("backups.manage") {
		if p := s.Apps.Plan(); p.Problem != "" {
			attention = append(attention, map[string]string{"backups": "1", "text": p.Problem})
		}
	}
	out := map[string]any{"apps": list, "attention": attention, "sharedCount": len(shared)}
	if u.Can("system.view") {
		out["system"] = s.sys.snapshot(s.C.Cfg.DataDir)
	}
	ok(w, out)
}

// --- store -----------------------------------------------------------------

func (s *Server) storeList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list := s.Cat.List(q.Get("q"), q.Get("cat"), q.Get("verified") == "1")
	installed := map[string]string{}
	all, _ := s.Apps.List()
	for _, a := range all {
		installed[a.StoreID] = a.ID
	}
	type item struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		ShortDesc string   `json:"shortDesc"`
		Verified  bool     `json:"verified"`
		Cats      []string `json:"categories"`
		Installed string   `json:"installed,omitempty"`
	}
	items := make([]item, 0, len(list))
	for _, a := range list {
		items = append(items, item{a.ID, a.Name, a.ShortDesc, a.Verified, a.Categories, installed[a.ID]})
	}
	ok(w, map[string]any{"apps": items, "categories": s.Cat.Categories(), "total": s.Cat.Count(), "synced": s.C.Setting("store_synced"), "loading": !s.Cat.Downloaded()})
}

func (s *Server) storeApp(w http.ResponseWriter, r *http.Request) {
	a, found := s.Cat.Get(r.PathValue("sid"))
	if !found {
		fail(w, 404, "That app isn't in the store.")
		return
	}
	ok(w, map[string]any{"app": a, "gpuAvailable": dirExists("/dev/dri")})
}

func (s *Server) icon(w http.ResponseWriter, r *http.Request) {
	p := s.Cat.LogoPath(r.PathValue("sid"))
	if p == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "max-age=86400")
	http.ServeFile(w, r, p)
}

func (s *Server) storeSync(w http.ResponseWriter, r *http.Request) {
	if err := s.Cat.Sync(); err != nil {
		failErr(w, err)
		return
	}
	s.log(r, "refreshed the store", "", "", true, "")
	ok(w, map[string]any{"total": s.Cat.Count()})
}

// --- install & jobs --------------------------------------------------------

func (s *Server) install(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in apps.InstallRequest
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	if !u.IsOwner && u.QuotaApps > 0 {
		var n int
		_ = s.C.DB.QueryRow(`SELECT COUNT(*) FROM apps WHERE installed_by=?`, u.ID).Scan(&n)
		if n >= u.QuotaApps {
			fail(w, 400, fmt.Sprintf("You've reached your limit of %d apps.", u.QuotaApps))
			return
		}
	}
	sa, found := s.Cat.Get(in.StoreID)
	if !found {
		fail(w, 404, "That app isn't in the store.")
		return
	}
	if sa.UsesDocker && !u.IsOwner {
		fail(w, 403, "This app controls Docker on the whole server, so only the owner can install it.")
		return
	}
	job, err := s.Apps.Install(in, u.ID)
	if err != nil {
		failErr(w, err)
		return
	}
	_ = s.Auth.GrantAllOnApp(u.ID, job.AppID)
	s.log(r, "installed", sa.Name, job.AppID, true, "")
	ok(w, map[string]string{"job": job.ID, "appId": job.AppID})
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	j, found := s.Apps.Job(r.PathValue("jid"))
	if !found {
		fail(w, 404, "That job is gone.")
		return
	}
	if !s.Auth.CanApp(userOf(r), j.AppID, "app.view") {
		fail(w, 403, "You don't have permission to see that.")
		return
	}
	ok(w, j)
}

// --- one app ---------------------------------------------------------------

func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	a, _ := s.Apps.Get(r.PathValue("id"))
	s.Apps.Status([]*apps.App{a})
	type folder struct {
		Key       string `json:"key"`
		Label     string `json:"label"`
		Root      string `json:"root"`
		RootLabel string `json:"rootLabel"`
		Path      string `json:"path"`
		Connected bool   `json:"connected"`
	}
	folders := []folder{}
	sa, inStore := s.Cat.Get(a.StoreID)
	roots := s.Store.Roots()
	for key, p := range a.Folders {
		f := folder{Key: key, Label: "Folder", Root: p.Root, Path: p.Path, Connected: s.Store.RootConnected(p.Root)}
		if a.Catalog == "custom" {
			f.Label = "The folder written as " + key
		}
		if inStore {
			for _, need := range sa.Folders {
				if need.Key == key {
					f.Label = need.Label
				}
			}
		}
		for _, rt := range roots {
			if rt.ID == p.Root {
				f.RootLabel = rt.Label
			}
		}
		folders = append(folders, f)
	}
	var tunnel map[string]any
	var tid int64
	var addr, pid string
	if s.C.DB.QueryRow(`SELECT id,address,playit_id FROM tunnels WHERE kind='app' AND app_id=?`, a.ID).Scan(&tid, &addr, &pid) == nil {
		tunnel = map[string]any{"id": tid, "address": addr, "ready": addr != ""}
	}
	people := []string{}
	users, _ := s.Auth.Users()
	for _, x := range users {
		if s.Auth.CanApp(x, a.ID, "app.open") {
			people = append(people, x.Username)
		}
	}
	if s.Playit.Linked() {
		s.Playit.RefreshIfStale()
	}
	out := map[string]any{
		"app": a, "folders": folders, "tunnel": tunnel, "people": people,
		"perms": s.Auth.AppPermsFor(u, a.ID), "gpuAvailable": dirExists("/dev/dri"), "playitLinked": s.Playit.Linked(),
		"playitPremium": s.Playit.Premium(), "secure": s.secureInfo(a.PublicPort()), "port": a.PublicPort(), "sharingNote": a.SharingNote,
		"version": a.Settings.Version, "update": s.Apps.UpdateFor(a), "undo": s.Apps.UndoFor(a), "undoDays": apps.UndoDays,
		"custom": a.Catalog == "custom", "canCompose": a.Catalog == "custom" && u.Can("apps.custom") && s.Auth.CanApp(u, a.ID, "app.settings"),
	}
	if inStore {
		out["store"] = map[string]any{"shortDesc": sa.ShortDesc, "version": sa.Version, "website": sa.Website, "verified": sa.Verified}
	}
	ok(w, out)
}

func (s *Server) appOpen(w http.ResponseWriter, r *http.Request) {
	a, _ := s.Apps.Get(r.PathValue("id"))
	if !isHomeHost(r.Host) {
		// Away from home the mini PC's own address doesn't work: use the app's shared address.
		// A secure address made on playit.gg comes first.
		if x := s.Playit.SecureFor(a.PublicPort()); x != nil && x.Problem == "" {
			ok(w, map[string]any{"url": x.URL() + a.URLSuffix, "frameable": false, "remote": true, "noGui": a.NoGUI})
			return
		}
		var addr string
		_ = s.C.DB.QueryRow(`SELECT address FROM tunnels WHERE kind='app' AND app_id=?`, a.ID).Scan(&addr)
		if addr == "" {
			fail(w, 400, "To open "+a.Name+" away from home, turn on Share online for it first (right-click the app › Share online).")
			return
		}
		ok(w, map[string]any{"url": a.Scheme() + "://" + addr + a.URLSuffix, "frameable": false, "remote": true, "noGui": a.NoGUI, "ownCert": a.HTTPS})
		return
	}
	s.Apps.CheckScheme(a)
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	url := fmt.Sprintf("%s://%s:%d%s", a.Scheme(), host, a.Port, a.URLSuffix)
	// A page opened over https can't show a plain http app inside a window, and
	// an app with a certificate of its own has to be let through by the browser first.
	ok(w, map[string]any{"url": url, "frameable": r.TLS == nil && !a.HTTPS && s.Apps.Frameable(a), "noGui": a.NoGUI, "ownCert": a.HTTPS})
}

func (s *Server) appAction(w http.ResponseWriter, r *http.Request, verb string, fn func(string) error) {
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	err := fn(id)
	s.log(r, verb, a.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) appStart(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "started", s.Apps.Start)
}
func (s *Server) appStop(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "stopped", s.Apps.Stop)
}
func (s *Server) appRestart(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "restarted", s.Apps.Restart)
}

func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	out, err := s.Apps.Logs(r.PathValue("id"), 300)
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]string{"log": out})
}

func (s *Server) appFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key  string `json:"key"`
		Root string `json:"root"`
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	id := r.PathValue("id")
	err := s.Apps.SetFolder(id, in.Key, apps.Picked{Root: in.Root, Path: in.Path})
	s.log(r, "changed a folder of", id, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) appGPU(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	id := r.PathValue("id")
	err := s.Apps.SetGPU(id, in.On)
	s.log(r, "changed the graphics chip option of", id, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) appRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	// stop sharing first
	rows, _ := s.C.DB.Query(`SELECT playit_id FROM tunnels WHERE app_id=?`, id)
	var pids []string
	for rows != nil && rows.Next() {
		var p string
		_ = rows.Scan(&p)
		pids = append(pids, p)
	}
	if rows != nil {
		rows.Close()
	}
	for _, p := range pids {
		_ = s.Playit.DeleteTunnel(p)
	}
	_, _ = s.C.DB.Exec(`DELETE FROM tunnels WHERE app_id=?`, id)
	err := s.Apps.Remove(id, r.URL.Query().Get("data") == "1")
	s.log(r, "removed", a.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

// --- who can use an app ----------------------------------------------------

func (s *Server) appAccess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	users, _ := s.Auth.Users()
	type row struct {
		ID       int64    `json:"id"`
		Username string   `json:"username"`
		Role     string   `json:"role"`
		Perms    []string `json:"perms"`
		Locked   bool     `json:"locked"` // owner/admin: always everything
	}
	out := []row{}
	for _, u := range users {
		locked := u.IsOwner || (u.Can("users.manage") && u.Can("system.manage"))
		out = append(out, row{u.ID, u.Username, u.RoleName, s.Auth.AppPermsFor(u, id), locked})
	}
	ok(w, map[string]any{"users": out, "perms": auth.AppPerms})
}

func (s *Server) appSetAccess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor := userOf(r)
	var in struct {
		UserID int64    `json:"userId"`
		Perms  []string `json:"perms"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	// you can only hand out what you have on this app
	mine := map[string]bool{}
	for _, p := range s.Auth.AppPermsFor(actor, id) {
		mine[p] = true
	}
	for _, p := range in.Perms {
		if !mine[p] {
			fail(w, 403, "You can only give permissions you have yourself.")
			return
		}
	}
	if err := s.Auth.SetAppPerms(in.UserID, id, in.Perms); err != nil {
		failErr(w, err)
		return
	}
	target, _ := s.Auth.UserByID(in.UserID)
	name := ""
	if target != nil {
		name = target.Username
	}
	s.log(r, "changed who can use", id+" ("+name+")", id, true, strings.Join(in.Perms, ","))
	ok(w, map[string]bool{"ok": true})
}

// --- helpers ---------------------------------------------------------------

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

var _ = storage.AppUID

// appUpdate moves an app to the newer version in the store, keeping a copy to go back to.
func (s *Server) appUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	var in struct {
		NoUndo bool `json:"noUndo"`
	}
	_ = readJSON(r, &in)
	job, err := s.Apps.Update(id, in.NoUndo)
	var ns *apps.ErrNoSpace
	if errors.As(err, &ns) {
		// the page asks whether to update without a way back
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "noSpace": true})
		return
	}
	s.log(r, "started an update", a.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]string{"job": job.ID, "appId": id})
}

func (s *Server) appUndo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	job, err := s.Apps.Undo(id)
	s.log(r, "started undoing an update", a.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]string{"job": job.ID, "appId": id})
}

func (s *Server) appDropUndo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	err := s.Apps.DropUndo(id)
	s.log(r, "deleted the copy for undoing an update", a.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

// appCheck asks right now whether a newer build of the app can be downloaded.
func (s *Server) appCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.Apps.CheckBuild(id)
	a, _ := s.Apps.Get(id)
	ok(w, map[string]any{"update": s.Apps.UpdateFor(a)})
}

// --- your own apps, from a compose file -----------------------------------

// customCheck reads a pasted compose file and says what will be asked.
func (s *Server) customCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Compose string `json:"compose"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	chk, _, err := apps.CheckCompose(in.Compose)
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, chk)
}

func (s *Server) customInstall(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in apps.CustomRequest
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	job, err := s.Apps.InstallCustom(in, u.ID)
	s.log(r, "added an app from a compose file", in.Name, "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	_ = s.Auth.GrantAllOnApp(u.ID, job.AppID)
	ok(w, map[string]string{"job": job.ID, "appId": job.AppID})
}

func (s *Server) customSource(w http.ResponseWriter, r *http.Request) {
	if !userOf(r).Can("apps.custom") {
		fail(w, 403, "You don't have permission to do that.")
		return
	}
	src, err := s.Apps.CustomSource(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, src)
}

func (s *Server) customEdit(w http.ResponseWriter, r *http.Request) {
	if !userOf(r).Can("apps.custom") {
		fail(w, 403, "You don't have permission to do that.")
		return
	}
	id := r.PathValue("id")
	var in struct {
		apps.CustomRequest
		NoUndo bool `json:"noUndo"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	job, err := s.Apps.EditCustom(id, in.CustomRequest, in.NoUndo)
	var ns *apps.ErrNoSpace
	if errors.As(err, &ns) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "noSpace": true})
		return
	}
	s.log(r, "changed the compose file of", in.Name, id, err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]string{"job": job.ID, "appId": id})
}
