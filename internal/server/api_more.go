package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MightyX3N/zeroos/internal/auth"
	"github.com/MightyX3N/zeroos/internal/storage"
)

// --- storage ---------------------------------------------------------------

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	ssd, _ := storage.UsageOf(s.C.Cfg.DataDir)
	out := map[string]any{"ssd": ssd, "canManage": u.Can("storage.manage")}
	shares, _ := s.Store.Shares()
	if shares == nil {
		shares = []*storage.Share{}
	}
	all, _ := s.Apps.List()
	for _, sh := range shares {
		root := fmt.Sprintf("nas:%d", sh.ID)
		for _, a := range all {
			for _, p := range a.Folders {
				if p.Root == root {
					sh.UsedBy = append(sh.UsedBy, a.Name)
					break
				}
			}
		}
	}
	out["shares"] = shares
	ok(w, out)
}

func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	ok(w, map[string]any{"found": storage.Discover(ctx)})
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	var in struct{ Host, Username, Password string }
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	names, err := storage.ListShares(strings.TrimSpace(in.Host), in.Username, in.Password)
	if err != nil {
		failErr(w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	ok(w, map[string]any{"shares": names})
}

func (s *Server) addShare(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Host, Share, Username, Password string }
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	sh, err := s.Store.AddShare(in.Name, strings.TrimSpace(in.Host), in.Share, in.Username, in.Password)
	s.log(r, "added a NAS share", in.Share, "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, sh)
}

func (s *Server) removeShare(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "sid")
	root := fmt.Sprintf("nas:%d", id)
	all, _ := s.Apps.List()
	for _, a := range all {
		for _, p := range a.Folders {
			if p.Root == root {
				fail(w, 400, a.Name+" uses this share. Change its folder or remove it first.")
				return
			}
		}
	}
	err := s.Store.RemoveShare(id)
	s.log(r, "removed a NAS share", strconv.FormatInt(id, 10), "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) reconnectShare(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Reconnect(pathInt(r, "sid")); err != nil {
		failErr(w, err)
		return
	}
	s.Apps.Reconcile()
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) shareLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	err := s.Store.UpdateLogin(pathInt(r, "sid"), in.Username, in.Password)
	s.log(r, "changed a NAS share login", "", "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	s.Apps.Reconcile()
	ok(w, map[string]bool{"ok": true})
}

// Folder picking is for people who may install apps or change an app's folders.
func (s *Server) canPickFolders(u *auth.User) bool {
	return u.Can("apps.install") || u.Can("storage.manage") || u.IsOwner || s.hasAnyAppPerm(u, "app.settings")
}

func (s *Server) hasAnyAppPerm(u *auth.User, perm string) bool {
	var n int
	_ = s.C.DB.QueryRow(`SELECT COUNT(*) FROM app_perms WHERE user_id=? AND perm=?`, u.ID, perm).Scan(&n)
	return n > 0
}

func (s *Server) folderRoots(w http.ResponseWriter, r *http.Request) {
	if !s.canPickFolders(userOf(r)) {
		fail(w, 403, "You don't have permission to do that.")
		return
	}
	ok(w, s.Store.Roots())
}

func (s *Server) folders(w http.ResponseWriter, r *http.Request) {
	if !s.canPickFolders(userOf(r)) {
		fail(w, 403, "You don't have permission to do that.")
		return
	}
	list, err := s.Store.Browse(r.URL.Query().Get("root"), r.URL.Query().Get("path"))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, list)
}

func (s *Server) makeFolder(w http.ResponseWriter, r *http.Request) {
	if !s.canPickFolders(userOf(r)) {
		fail(w, 403, "You don't have permission to do that.")
		return
	}
	var in struct{ Root, Path, Name string }
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	if err := s.Store.MakeFolder(in.Root, in.Path, in.Name); err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

// --- sharing online (playit) -----------------------------------------------

type tunnelRow struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	AppID     string `json:"appId,omitempty"`
	StoreID   string `json:"storeId,omitempty"`
	Name      string `json:"name"`
	LocalPort int    `json:"localPort,omitempty"`
	Proto     string `json:"proto"`
	Address   string `json:"address"`
	Secure    string `json:"secure,omitempty"` // https address made on playit.gg
	CanRemove bool   `json:"canRemove"`
}

func (s *Server) tunnels(u *userView) []tunnelRow {
	rows, err := s.C.DB.Query(`SELECT t.id,t.kind,COALESCE(t.app_id,''),COALESCE(a.store_id,''),t.name,t.local_port,t.proto,t.address,COALESCE(t.created_by,0) FROM tunnels t LEFT JOIN apps a ON a.id=t.app_id WHERE t.kind<>'zeroos' ORDER BY t.kind, t.name`)
	if err != nil {
		return []tunnelRow{}
	}
	defer rows.Close()
	out := []tunnelRow{}
	for rows.Next() {
		var t tunnelRow
		var by int64
		_ = rows.Scan(&t.ID, &t.Kind, &t.AppID, &t.StoreID, &t.Name, &t.LocalPort, &t.Proto, &t.Address, &by)
		if t.Kind == "app" {
			if !s.Auth.CanApp(u.User, t.AppID, "app.view") {
				continue
			}
			t.LocalPort = 0 // never show ports for apps
			t.CanRemove = s.Auth.CanApp(u.User, t.AppID, "app.share")
		} else {
			if !u.Can("playit.tunnels.all") && by != u.ID {
				continue
			}
			t.CanRemove = true
		}
		out = append(out, t)
	}
	// Apps with a secure address made on playit.gg, shared the plain way or not.
	seen := map[string]int{}
	for i, t := range out {
		if t.Kind == "app" {
			seen[t.AppID] = i + 1
		}
	}
	for _, a := range s.appList() {
		x := s.Playit.SecureFor(a.PublicPort())
		if x == nil || !s.Auth.CanApp(u.User, a.ID, "app.view") {
			continue
		}
		if i := seen[a.ID]; i > 0 {
			out[i-1].Secure = x.URL()
		} else {
			out = append(out, tunnelRow{Kind: "app", AppID: a.ID, StoreID: a.StoreID, Name: a.Name, Proto: "tcp", Secure: x.URL()})
		}
	}
	return out
}

// sharingRefresh asks playit again, for "Check now" after making a tunnel on playit.gg.
func (s *Server) sharingRefresh(w http.ResponseWriter, r *http.Request) {
	s.Playit.Refresh()
	s.refreshTunnelAddresses()
	ok(w, map[string]bool{"ok": true})
}

type userView struct{ *auth.User }

func (s *Server) sharing(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	ok(w, map[string]any{
		"playit":     s.Playit.InfoCached(),
		"tunnels":    s.tunnels(&userView{u}),
		"canAccount": u.Can("playit.account"),
		"canPorts":   u.Can("playit.tunnels.all") || u.QuotaTunnels > 0,
		"remote":     s.remoteInfo(),
		"canRemote":  u.Can("system.manage"),
		"twoStep":    u.TOTP,
	})
}

func (s *Server) claimStart(w http.ResponseWriter, r *http.Request) {
	url := s.Playit.StartClaim()
	ok(w, map[string]string{"url": url})
}

func (s *Server) claimPoll(w http.ResponseWriter, r *http.Request) {
	state, err := s.Playit.PollClaim()
	if err != nil {
		failErr(w, err)
		return
	}
	if state == "linked" {
		s.log(r, "linked playit", "", "", true, "")
	}
	ok(w, map[string]string{"state": state})
}

func (s *Server) claimKey(w http.ResponseWriter, r *http.Request) {
	var in struct{ Secret string }
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	err := s.Playit.SetSecret(in.Secret)
	s.log(r, "linked playit with a key", "", "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request) {
	err := s.Playit.Unlink()
	s.log(r, "unlinked playit", "", "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) tunnelQuotaOK(u *auth.User) error {
	if u.Can("playit.tunnels.all") {
		return nil
	}
	if u.QuotaTunnels <= 0 {
		return errors.New("You don't have permission to share online.")
	}
	var n int
	_ = s.C.DB.QueryRow(`SELECT COUNT(*) FROM tunnels WHERE created_by=?`, u.ID).Scan(&n)
	if n >= u.QuotaTunnels {
		return fmt.Errorf("You've reached your limit of %d shares.", u.QuotaTunnels)
	}
	return nil
}

func (s *Server) appShare(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id := r.PathValue("id")
	a, _ := s.Apps.Get(id)
	var in struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	var tid int64
	var pid string
	exists := s.C.DB.QueryRow(`SELECT id,playit_id FROM tunnels WHERE kind='app' AND app_id=?`, id).Scan(&tid, &pid) == nil
	if in.On && !exists {
		pid, err := s.Playit.CreateTunnel("ZeroOS - "+a.Name, a.PublicPort(), "tcp")
		s.log(r, "shared online", a.Name, id, err == nil, errText(err))
		if err != nil {
			failErr(w, err)
			return
		}
		_, _ = s.C.DB.Exec(`INSERT INTO tunnels(kind,app_id,name,local_port,proto,playit_id,created_by,created) VALUES('app',?,?,?,?,?,?,?)`, id, a.Name, a.PublicPort(), "tcp", pid, u.ID, time.Now().Unix())
		go s.chaseAddresses()
	}
	if !in.On && exists {
		err := s.Playit.DeleteTunnel(pid)
		s.log(r, "stopped sharing online", a.Name, id, err == nil, errText(err))
		_, _ = s.C.DB.Exec(`DELETE FROM tunnels WHERE id=?`, tid)
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) sharePort(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	if err := s.tunnelQuotaOK(u); err != nil {
		fail(w, 403, err.Error())
		return
	}
	var in struct {
		Name  string `json:"name"`
		Port  int    `json:"port"`
		Proto string `json:"proto"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || in.Port < 1 || in.Port > 65535 {
		fail(w, 400, "Give it a name and a port between 1 and 65535.")
		return
	}
	if in.Proto != "tcp" && in.Proto != "udp" && in.Proto != "both" {
		in.Proto = "both"
	}
	pid, err := s.Playit.CreateTunnel("ZeroOS - "+in.Name, in.Port, in.Proto)
	s.log(r, "shared a port online", fmt.Sprintf("%s (%d)", in.Name, in.Port), "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	_, _ = s.C.DB.Exec(`INSERT INTO tunnels(kind,name,local_port,proto,playit_id,created_by,created) VALUES('port',?,?,?,?,?,?)`, in.Name, in.Port, in.Proto, pid, u.ID, time.Now().Unix())
	go s.chaseAddresses()
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) removeTunnel(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id := pathInt(r, "tid")
	var kind, appID, pid, name string
	var by int64
	err := s.C.DB.QueryRow(`SELECT kind,COALESCE(app_id,''),playit_id,name,COALESCE(created_by,0) FROM tunnels WHERE id=?`, id).Scan(&kind, &appID, &pid, &name, &by)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "That share is already gone.")
		return
	}
	allowed := u.Can("playit.tunnels.all") || (kind == "port" && by == u.ID) || (kind == "app" && s.Auth.CanApp(u, appID, "app.share"))
	if !allowed {
		fail(w, 403, "You don't have permission to do that.")
		return
	}
	err = s.Playit.DeleteTunnel(pid)
	_, _ = s.C.DB.Exec(`DELETE FROM tunnels WHERE id=?`, id)
	s.log(r, "stopped sharing online", name, appID, err == nil, errText(err))
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) refreshTunnelAddresses() {
	if !s.Playit.Linked() {
		return
	}
	exist, addrs, err := s.Playit.Tunnels()
	if err != nil {
		return
	}
	for pid, addr := range addrs {
		_, _ = s.C.DB.Exec(`UPDATE tunnels SET address=? WHERE playit_id=? AND address<>?`, addr, pid, addr)
	}
	s.dropDeletedTunnels(exist)
}

// dropDeletedTunnels forgets tunnels that someone deleted on playit.gg itself,
// so ZeroOS doesn't keep showing an address that no longer works. To be sure,
// a tunnel has to be a few minutes old and missing three checks in a row.
func (s *Server) dropDeletedTunnels(exist map[string]bool) {
	s.goneMu.Lock()
	defer s.goneMu.Unlock()
	if time.Since(s.goneSeen) < 20*time.Second {
		return
	}
	s.goneSeen = time.Now()
	type row struct {
		id          int64
		pid, name   string
		appID, kind string
	}
	var list []row
	rows, err := s.C.DB.Query(`SELECT id,playit_id,name,COALESCE(app_id,''),kind FROM tunnels WHERE playit_id<>'' AND created<?`, time.Now().Add(-2*time.Minute).Unix())
	if err != nil {
		return
	}
	for rows.Next() {
		var r row
		_ = rows.Scan(&r.id, &r.pid, &r.name, &r.appID, &r.kind)
		list = append(list, r)
	}
	rows.Close()
	next := map[string]int{}
	for _, r := range list {
		if exist[r.pid] {
			continue
		}
		if next[r.pid] = s.gone[r.pid] + 1; next[r.pid] < 3 {
			continue
		}
		delete(next, r.pid)
		_, _ = s.C.DB.Exec(`DELETE FROM tunnels WHERE id=?`, r.id)
		what := "stopped sharing online (the tunnel was deleted on playit.gg)"
		if r.kind == "zeroos" {
			what = "turned off reaching ZeroOS from outside (the tunnel was deleted on playit.gg)"
		}
		s.C.Log("system", what, r.name, r.appID, true, "")
	}
	s.gone = next
}

// chaseAddresses asks playit for the address of new tunnels until each has
// one. playit hands the address out a moment after the tunnel is made.
func (s *Server) chaseAddresses() {
	for _, wait := range []time.Duration{0, 2, 3, 5, 10, 20, 40} {
		time.Sleep(wait * time.Second)
		s.refreshTunnelAddresses()
		var missing int
		_ = s.C.DB.QueryRow(`SELECT COUNT(*) FROM tunnels WHERE address=''`).Scan(&missing)
		if missing == 0 {
			return
		}
	}
}

// --- users & roles ---------------------------------------------------------

func (s *Server) permList(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"global": auth.GlobalPerms, "app": auth.AppPerms})
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	list, err := s.Auth.Users()
	if err != nil {
		failErr(w, err)
		return
	}
	type row struct {
		*auth.User
		Perms []string `json:"perms"`
	}
	out := []row{}
	for _, u := range list {
		out = append(out, row{u, u.Perms()})
	}
	ok(w, out)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in auth.NewUser
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	id, err := s.Auth.CreateUser(userOf(r), in)
	s.log(r, "added a user", in.Username, "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]int64{"id": id})
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var in auth.NewUser
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	id := pathInt(r, "uid")
	err := s.Auth.UpdateUser(userOf(r), id, in)
	s.log(r, "changed a user", in.Username, "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "uid")
	if id == userOf(r).ID {
		fail(w, 400, "You can't delete yourself.")
		return
	}
	target, _ := s.Auth.UserByID(id)
	err := s.Auth.DeleteUser(id)
	name := ""
	if target != nil {
		name = target.Username
	}
	s.log(r, "deleted a user", name, "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Password string }
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	id := pathInt(r, "uid")
	target, err := s.Auth.UserByID(id)
	if err != nil {
		fail(w, 404, "User not found.")
		return
	}
	if target.IsOwner && !userOf(r).IsOwner {
		fail(w, 403, "Only the owner can change the owner's password.")
		return
	}
	err = s.Auth.SetPassword(id, in.Password)
	s.log(r, "reset the password of", target.Username, "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) userApps(w http.ResponseWriter, r *http.Request) {
	target, err := s.Auth.UserByID(pathInt(r, "uid"))
	if err != nil {
		fail(w, 404, "User not found.")
		return
	}
	list, _ := s.Apps.List()
	type row struct {
		ID      string   `json:"id"`
		StoreID string   `json:"storeId"`
		Name    string   `json:"name"`
		Perms   []string `json:"perms"`
	}
	out := []row{}
	for _, a := range list {
		out = append(out, row{a.ID, a.StoreID, a.Name, s.Auth.AppPermsFor(target, a.ID)})
	}
	ok(w, map[string]any{"apps": out, "locked": target.IsOwner || (target.Can("users.manage") && target.Can("system.manage"))})
}

func (s *Server) setUserApps(w http.ResponseWriter, r *http.Request) {
	actor := userOf(r)
	id := pathInt(r, "uid")
	var in map[string][]string
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	for appID, perms := range in {
		if _, err := s.Apps.Get(appID); err != nil {
			continue
		}
		mine := map[string]bool{}
		for _, p := range s.Auth.AppPermsFor(actor, appID) {
			mine[p] = true
		}
		clean := []string{}
		for _, p := range perms {
			if mine[p] {
				clean = append(clean, p)
			}
		}
		if err := s.Auth.SetAppPerms(id, appID, clean); err != nil {
			failErr(w, err)
			return
		}
	}
	target, _ := s.Auth.UserByID(id)
	if target != nil {
		s.log(r, "changed which apps a user has", target.Username, "", true, "")
	}
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) roles(w http.ResponseWriter, r *http.Request) {
	list, err := s.Auth.Roles()
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, list)
}

func (s *Server) saveRole(w http.ResponseWriter, r *http.Request) {
	var in auth.Role
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	if r.PathValue("rid") != "" {
		in.ID = pathInt(r, "rid")
	}
	actor := userOf(r)
	if !actor.IsOwner {
		have := map[string]bool{}
		for _, p := range actor.Perms() {
			have[p] = true
		}
		for _, p := range in.Perms {
			if !have[p] {
				fail(w, 403, "You can only give permissions you have yourself.")
				return
			}
		}
	}
	id, err := s.Auth.SaveRole(in)
	s.log(r, "saved a role", in.Name, "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]int64{"id": id})
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	err := s.Auth.DeleteRole(pathInt(r, "rid"))
	s.log(r, "deleted a role", r.PathValue("rid"), "", err == nil, errText(err))
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

// --- activity --------------------------------------------------------------

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT ts,user,action,target,app_id,ok,detail FROM activity`
	args := []any{}
	if app := r.URL.Query().Get("app"); app != "" {
		if !s.Auth.CanApp(u, app, "app.view") {
			fail(w, 403, "You don't have permission to see that.")
			return
		}
		q += ` WHERE app_id=?`
		args = append(args, app)
	} else if !u.Can("system.view") {
		q += ` WHERE user=?`
		args = append(args, u.Username)
	}
	q += ` ORDER BY id DESC LIMIT 300`
	rows, err := s.C.DB.Query(q, args...)
	if err != nil {
		failErr(w, err)
		return
	}
	defer rows.Close()
	type row struct {
		TS     int64  `json:"ts"`
		User   string `json:"user"`
		Action string `json:"action"`
		Target string `json:"target"`
		AppID  string `json:"appId"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	out := []row{}
	for rows.Next() {
		var x row
		_ = rows.Scan(&x.TS, &x.User, &x.Action, &x.Target, &x.AppID, &x.OK, &x.Detail)
		out = append(out, x)
	}
	ok(w, out)
}

// --- system ----------------------------------------------------------------

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	snap := s.sys.snapshot(s.C.Cfg.DataDir)
	snap["addresses"] = s.WebAddresses()
	ok(w, snap)
}
