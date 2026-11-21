// Package auth handles accounts, passwords, sessions, roles and permissions.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
	"golang.org/x/crypto/argon2"
)

// Global permissions cover the whole server.
var GlobalPerms = []PermInfo{
	{"apps.install", "Get apps from the Store"},
	{"store.repos", "Manage store catalogs"},
	{"storage.manage", "Manage Space and NAS shares"},
	{"playit.account", "Link or unlink playit"},
	{"playit.tunnels.all", "Share ports online and manage every tunnel"},
	{"users.manage", "Manage users"},
	{"roles.manage", "Manage roles"},
	{"system.view", "See system stats and the full activity log"},
	{"system.manage", "Update ZeroOS and change system settings"},
	{"backups.manage", "Manage backups for everything"},
}

// AppPerms cover one app.
var AppPerms = []PermInfo{
	{"app.view", "See it on their desktop"},
	{"app.open", "Open it"},
	{"app.power", "Start, stop, restart"},
	{"app.settings", "Change its settings and folders"},
	{"app.share", "Share it online"},
	{"app.backups", "Make and restore backups"},
	{"app.update", "Update it"},
	{"app.access", "Give others access to it"},
	{"app.remove", "Remove it"},
}

type PermInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

func allNames(list []PermInfo) []string {
	out := make([]string, len(list))
	for i, p := range list {
		out[i] = p.Name
	}
	return out
}

func isGlobal(p string) bool {
	for _, g := range GlobalPerms {
		if g.Name == p {
			return true
		}
	}
	return false
}

func isAppPerm(p string) bool {
	for _, g := range AppPerms {
		if g.Name == p {
			return true
		}
	}
	return false
}

// Auth is the account service.
type Auth struct{ c *core.Core }

func New(c *core.Core) (*Auth, error) {
	a := &Auth{c: c}
	return a, a.ensureRoles()
}

// ensureRoles creates the editable default roles once.
func (a *Auth) ensureRoles() error {
	var n int
	if err := a.c.DB.QueryRow(`SELECT COUNT(*) FROM roles`).Scan(&n); err != nil || n > 0 {
		return err
	}
	type role struct {
		name    string
		perms   []string
		appPerm []string
	}
	all := allNames(GlobalPerms)
	roles := []role{
		{"Admin", all, allNames(AppPerms)},
		{"Member", []string{}, []string{"app.view", "app.open"}},
		{"Installer", []string{"apps.install"}, []string{"app.view", "app.open"}},
	}
	for _, r := range roles {
		p, _ := json.Marshal(r.perms)
		ap, _ := json.Marshal(r.appPerm)
		if _, err := a.c.DB.Exec(`INSERT INTO roles(name,perms,app_perms,builtin) VALUES(?,?,?,1)`, r.name, string(p), string(ap)); err != nil {
			return err
		}
	}
	return nil
}

// --- passwords -------------------------------------------------------------

func HashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	h := argon2.IDKey([]byte(pw), salt, 2, 64*1024, 2, 32)
	return fmt.Sprintf("argon2id$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h))
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[1])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, 2, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{1,31}$`)

// ValidateNewPassword enforces a minimum that stays friendly.
func ValidateNewPassword(pw string) error {
	if len([]rune(pw)) < 8 {
		return errors.New("Use at least 8 characters for the password.")
	}
	return nil
}

// --- setup -----------------------------------------------------------------

// SetupDone tells whether the owner account exists.
func (a *Auth) SetupDone() bool {
	var n int
	_ = a.c.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE is_owner=1`).Scan(&n)
	return n > 0
}

// SetupCode returns the one-time setup code, creating it (and writing it to a
// root-only file) if needed. Only shown in the terminal, never on the web.
func (a *Auth) SetupCode() string {
	code := a.c.Setting("setup_code")
	if code == "" {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		code = strings.ToUpper(hex.EncodeToString(b))
		_ = a.c.SetSetting("setup_code", code)
	}
	_ = os.WriteFile(filepath.Join(a.c.Cfg.DataDir, "setup-code"), []byte(code+"\n"), 0o600)
	return code
}

// CheckSetupCode compares a typed code to the stored one.
func (a *Auth) CheckSetupCode(code string) bool {
	want := a.c.Setting("setup_code")
	code = strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(code, "-", "")))
	return want != "" && subtle.ConstantTimeCompare([]byte(code), []byte(want)) == 1
}

// CreateOwner makes the first account. Fails once an owner exists.
func (a *Auth) CreateOwner(username, password string) (int64, error) {
	if a.SetupDone() {
		return 0, errors.New("This server already has an owner.")
	}
	if !usernameRe.MatchString(username) {
		return 0, errors.New("Use 2–32 letters, numbers, dots, dashes or underscores for the username.")
	}
	if err := ValidateNewPassword(password); err != nil {
		return 0, err
	}
	res, err := a.c.DB.Exec(`INSERT INTO users(username,pass_hash,is_owner,created) VALUES(?,?,1,?)`, username, HashPassword(password), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	_ = os.Remove(filepath.Join(a.c.Cfg.DataDir, "setup-code"))
	_ = a.c.SetSetting("setup_code", "")
	return res.LastInsertId()
}

// --- users -----------------------------------------------------------------

type User struct {
	ID           int64    `json:"id"`
	Username     string   `json:"username"`
	IsOwner      bool     `json:"isOwner"`
	RoleID       int64    `json:"roleId"`
	RoleName     string   `json:"role"`
	ExtraPerms   []string `json:"extraPerms"`
	RemovedPerms []string `json:"removedPerms"`
	QuotaApps    int      `json:"quotaApps"`
	QuotaTunnels int      `json:"quotaTunnels"`
	LastSeen     int64    `json:"lastSeen"`
	TOTP         bool     `json:"totp"`
	rolePerms    []string
	roleAppPerms []string
}

const userCols = `u.id,u.username,u.is_owner,COALESCE(u.role_id,0),COALESCE(r.name,''),u.extra_perms,u.removed_perms,u.quota_apps,u.quota_tunnels,u.last_seen,u.totp_secret<>'',COALESCE(r.perms,'[]'),COALESCE(r.app_perms,'[]')`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var extra, removed, rp, rap string
	if err := row.Scan(&u.ID, &u.Username, &u.IsOwner, &u.RoleID, &u.RoleName, &extra, &removed, &u.QuotaApps, &u.QuotaTunnels, &u.LastSeen, &u.TOTP, &rp, &rap); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(extra), &u.ExtraPerms)
	_ = json.Unmarshal([]byte(removed), &u.RemovedPerms)
	_ = json.Unmarshal([]byte(rp), &u.rolePerms)
	_ = json.Unmarshal([]byte(rap), &u.roleAppPerms)
	if u.IsOwner {
		u.RoleName = "Owner"
	}
	if u.ExtraPerms == nil {
		u.ExtraPerms = []string{}
	}
	if u.RemovedPerms == nil {
		u.RemovedPerms = []string{}
	}
	return u, nil
}

func (a *Auth) UserByID(id int64) (*User, error) {
	return scanUser(a.c.DB.QueryRow(`SELECT `+userCols+` FROM users u LEFT JOIN roles r ON r.id=u.role_id WHERE u.id=?`, id))
}

func (a *Auth) UserByName(name string) (*User, error) {
	return scanUser(a.c.DB.QueryRow(`SELECT `+userCols+` FROM users u LEFT JOIN roles r ON r.id=u.role_id WHERE u.username=?`, name))
}

func (a *Auth) Users() ([]*User, error) {
	rows, err := a.c.DB.Query(`SELECT ` + userCols + ` FROM users u LEFT JOIN roles r ON r.id=u.role_id ORDER BY u.is_owner DESC, u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// Perms returns the user's effective global permissions.
func (u *User) Perms() []string {
	if u.IsOwner {
		return allNames(GlobalPerms)
	}
	set := map[string]bool{}
	for _, p := range u.rolePerms {
		set[p] = true
	}
	for _, p := range u.ExtraPerms {
		set[p] = true
	}
	for _, p := range u.RemovedPerms {
		delete(set, p)
	}
	out := []string{}
	for p := range set {
		if isGlobal(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Can checks a global permission.
func (u *User) Can(perm string) bool {
	if u.IsOwner {
		return true
	}
	for _, p := range u.Perms() {
		if p == perm {
			return true
		}
	}
	return false
}

// RoleAppPerms are the app permissions a role grants on apps shared with the user.
func (u *User) RoleAppPerms() []string { return u.roleAppPerms }

// CanApp checks a per-app permission.
func (a *Auth) CanApp(u *User, appID, perm string) bool {
	if u.IsOwner {
		return true
	}
	// Admin role: every app permission everywhere.
	if u.Can("users.manage") && u.Can("system.manage") {
		for _, p := range u.roleAppPerms {
			if p == perm {
				return true
			}
		}
	}
	var n int
	_ = a.c.DB.QueryRow(`SELECT COUNT(*) FROM app_perms WHERE user_id=? AND app_id=? AND perm=?`, u.ID, appID, perm).Scan(&n)
	return n > 0
}

// AppPermsFor lists a user's permissions on one app.
func (a *Auth) AppPermsFor(u *User, appID string) []string {
	out := []string{}
	for _, p := range AppPerms {
		if a.CanApp(u, appID, p.Name) {
			out = append(out, p.Name)
		}
	}
	return out
}

// SetAppPerms replaces one user's permissions on one app.
func (a *Auth) SetAppPerms(userID int64, appID string, perms []string) error {
	tx, err := a.c.DB.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM app_perms WHERE user_id=? AND app_id=?`, userID, appID); err != nil {
		tx.Rollback()
		return err
	}
	for _, p := range perms {
		if !isAppPerm(p) {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO app_perms(user_id,app_id,perm) VALUES(?,?,?)`, userID, appID, p); err != nil {
			tx.Rollback()
			return err
		}
	}
	// Seeing an app is implied by any other permission on it.
	if len(perms) > 0 {
		_, _ = tx.Exec(`INSERT OR IGNORE INTO app_perms(user_id,app_id,perm) VALUES(?,?,'app.view')`, userID, appID)
	}
	return tx.Commit()
}

// GrantAllOnApp gives the installer every permission on the app.
func (a *Auth) GrantAllOnApp(userID int64, appID string) error {
	return a.SetAppPerms(userID, appID, allNames(AppPerms))
}

type NewUser struct {
	Username     string   `json:"username"`
	Password     string   `json:"password"`
	RoleID       int64    `json:"roleId"`
	ExtraPerms   []string `json:"extraPerms"`
	RemovedPerms []string `json:"removedPerms"`
	QuotaApps    int      `json:"quotaApps"`
	QuotaTunnels int      `json:"quotaTunnels"`
}

// CreateUser adds a user. The actor may only hand out permissions they hold.
func (a *Auth) CreateUser(actor *User, nu NewUser) (int64, error) {
	if !usernameRe.MatchString(nu.Username) {
		return 0, errors.New("Use 2–32 letters, numbers, dots, dashes or underscores for the username.")
	}
	if err := ValidateNewPassword(nu.Password); err != nil {
		return 0, err
	}
	if err := a.checkGrantable(actor, nu.RoleID, nu.ExtraPerms); err != nil {
		return 0, err
	}
	extra, _ := json.Marshal(cleanPerms(nu.ExtraPerms))
	removed, _ := json.Marshal(cleanPerms(nu.RemovedPerms))
	res, err := a.c.DB.Exec(`INSERT INTO users(username,pass_hash,role_id,extra_perms,removed_perms,quota_apps,quota_tunnels,created) VALUES(?,?,?,?,?,?,?,?)`,
		nu.Username, HashPassword(nu.Password), nullID(nu.RoleID), string(extra), string(removed), nu.QuotaApps, nu.QuotaTunnels, time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, errors.New("That username is already taken.")
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateUser changes role, extra permissions and limits.
func (a *Auth) UpdateUser(actor *User, id int64, nu NewUser) error {
	target, err := a.UserByID(id)
	if err != nil {
		return errors.New("User not found.")
	}
	if target.IsOwner {
		return errors.New("The owner always has every permission.")
	}
	if err := a.checkGrantable(actor, nu.RoleID, nu.ExtraPerms); err != nil {
		return err
	}
	extra, _ := json.Marshal(cleanPerms(nu.ExtraPerms))
	removed, _ := json.Marshal(cleanPerms(nu.RemovedPerms))
	_, err = a.c.DB.Exec(`UPDATE users SET role_id=?,extra_perms=?,removed_perms=?,quota_apps=?,quota_tunnels=? WHERE id=?`,
		nullID(nu.RoleID), string(extra), string(removed), nu.QuotaApps, nu.QuotaTunnels, id)
	return err
}

func (a *Auth) DeleteUser(id int64) error {
	u, err := a.UserByID(id)
	if err != nil {
		return errors.New("User not found.")
	}
	if u.IsOwner {
		return errors.New("The owner can't be deleted.")
	}
	_, err = a.c.DB.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

// VerifyPassword checks a user's current password.
func (a *Auth) VerifyPassword(id int64, pw string) bool {
	var hash string
	if a.c.DB.QueryRow(`SELECT pass_hash FROM users WHERE id=?`, id).Scan(&hash) != nil {
		return false
	}
	return checkPassword(hash, pw)
}

// SetPassword sets a new password and signs the user out everywhere else.
func (a *Auth) SetPassword(id int64, pw string) error {
	if err := ValidateNewPassword(pw); err != nil {
		return err
	}
	if _, err := a.c.DB.Exec(`UPDATE users SET pass_hash=?, failed_logins=0, locked_until=0 WHERE id=?`, HashPassword(pw), id); err != nil {
		return err
	}
	_, err := a.c.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	return err
}

func (a *Auth) checkGrantable(actor *User, roleID int64, extra []string) error {
	if actor.IsOwner {
		return nil
	}
	have := map[string]bool{}
	for _, p := range actor.Perms() {
		have[p] = true
	}
	perms := append([]string{}, extra...)
	if roleID > 0 {
		r, err := a.Role(roleID)
		if err != nil {
			return errors.New("Role not found.")
		}
		perms = append(perms, r.Perms...)
	}
	for _, p := range perms {
		if !have[p] {
			return fmt.Errorf("You can only give permissions you have yourself (%s).", p)
		}
	}
	return nil
}

func cleanPerms(in []string) []string {
	out := []string{}
	for _, p := range in {
		if isGlobal(p) {
			out = append(out, p)
		}
	}
	return out
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// --- roles -----------------------------------------------------------------

type Role struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Perms    []string `json:"perms"`
	AppPerms []string `json:"appPerms"`
	Builtin  bool     `json:"builtin"`
	Users    int      `json:"users"`
}

func (a *Auth) Role(id int64) (*Role, error) {
	r := &Role{}
	var p, ap string
	err := a.c.DB.QueryRow(`SELECT id,name,perms,app_perms,builtin,(SELECT COUNT(*) FROM users WHERE role_id=roles.id) FROM roles WHERE id=?`, id).Scan(&r.ID, &r.Name, &p, &ap, &r.Builtin, &r.Users)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(p), &r.Perms)
	_ = json.Unmarshal([]byte(ap), &r.AppPerms)
	return r, nil
}

func (a *Auth) Roles() ([]*Role, error) {
	rows, err := a.c.DB.Query(`SELECT id FROM roles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []*Role
	for _, id := range ids {
		r, err := a.Role(id)
		if err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (a *Auth) SaveRole(r Role) (int64, error) {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return 0, errors.New("Give the role a name.")
	}
	ap := []string{}
	for _, p := range r.AppPerms {
		if isAppPerm(p) {
			ap = append(ap, p)
		}
	}
	p, _ := json.Marshal(cleanPerms(r.Perms))
	apj, _ := json.Marshal(ap)
	if r.ID == 0 {
		res, err := a.c.DB.Exec(`INSERT INTO roles(name,perms,app_perms) VALUES(?,?,?)`, r.Name, string(p), string(apj))
		if err != nil {
			return 0, errors.New("A role with that name already exists.")
		}
		return res.LastInsertId()
	}
	_, err := a.c.DB.Exec(`UPDATE roles SET name=?,perms=?,app_perms=? WHERE id=?`, r.Name, string(p), string(apj), r.ID)
	return r.ID, err
}

func (a *Auth) DeleteRole(id int64) error {
	r, err := a.Role(id)
	if err != nil {
		return errors.New("Role not found.")
	}
	if r.Users > 0 {
		return errors.New("Move the users in this role to another role first.")
	}
	_, err = a.c.DB.Exec(`DELETE FROM roles WHERE id=?`, id)
	return err
}

// --- sessions & login ------------------------------------------------------

var ErrBadLogin = errors.New("Wrong username or password.")
var ErrLocked = errors.New("Too many wrong tries. Wait a few minutes and try again.")
var ErrNeedCode = errors.New("Enter the 6-digit code from your authenticator app.")

// Login checks the password (with lockout) and returns a new session token.
func (a *Auth) Login(username, password, code, agent string, remember bool) (string, *User, error) {
	var id int64
	var hash string
	var failed int
	var lockedUntil int64
	err := a.c.DB.QueryRow(`SELECT id,pass_hash,failed_logins,locked_until FROM users WHERE username=?`, username).Scan(&id, &hash, &failed, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		HashPassword(password) // same time as a real check
		return "", nil, ErrBadLogin
	}
	if err != nil {
		return "", nil, err
	}
	now := time.Now().Unix()
	if lockedUntil > now {
		return "", nil, ErrLocked
	}
	if !checkPassword(hash, password) {
		failed++
		lock := int64(0)
		if failed >= 5 {
			lock = now + 300
			failed = 0
		}
		_, _ = a.c.DB.Exec(`UPDATE users SET failed_logins=?, locked_until=? WHERE id=?`, failed, lock, id)
		return "", nil, ErrBadLogin
	}
	if secret := a.totpSecret(id); secret != "" {
		if code == "" {
			return "", nil, ErrNeedCode
		}
		if !CheckTOTP(secret, code) {
			return "", nil, errors.New("That code didn't match. Try the newest code from your app.")
		}
	}
	_, _ = a.c.DB.Exec(`UPDATE users SET failed_logins=0, locked_until=0, last_seen=? WHERE id=?`, now, id)
	life := int64(12 * 3600)
	if remember {
		life = 30 * 24 * 3600
	}
	tok := core.RandomHex(32)
	if _, err := a.c.DB.Exec(`INSERT INTO sessions(token_hash,user_id,created,expires,agent) VALUES(?,?,?,?,?)`, hashToken(tok), id, now, now+life, agent); err != nil {
		return "", nil, err
	}
	u, err := a.UserByID(id)
	return tok, u, err
}

// NewSession creates a session directly (used right after setup).
func (a *Auth) NewSession(userID int64, agent string) (string, error) {
	now := time.Now().Unix()
	tok := core.RandomHex(32)
	_, err := a.c.DB.Exec(`INSERT INTO sessions(token_hash,user_id,created,expires,agent) VALUES(?,?,?,?,?)`, hashToken(tok), userID, now, now+12*3600, agent)
	return tok, err
}

// Session finds the user behind a session token.
func (a *Auth) Session(tok string) (*User, error) {
	if tok == "" {
		return nil, errors.New("no session")
	}
	var uid, exp int64
	err := a.c.DB.QueryRow(`SELECT user_id,expires FROM sessions WHERE token_hash=?`, hashToken(tok)).Scan(&uid, &exp)
	if err != nil || exp < time.Now().Unix() {
		return nil, errors.New("session expired")
	}
	_, _ = a.c.DB.Exec(`UPDATE users SET last_seen=? WHERE id=?`, time.Now().Unix(), uid)
	return a.UserByID(uid)
}

func (a *Auth) Logout(tok string) {
	_, _ = a.c.DB.Exec(`DELETE FROM sessions WHERE token_hash=?`, hashToken(tok))
}

type SessionInfo struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Agent   string `json:"agent"`
	Current bool   `json:"current"`
}

func (a *Auth) Sessions(userID int64, current string) []SessionInfo {
	rows, err := a.c.DB.Query(`SELECT token_hash,created,agent FROM sessions WHERE user_id=? AND expires>? ORDER BY created DESC`, userID, time.Now().Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	cur := hashToken(current)
	out := []SessionInfo{}
	for rows.Next() {
		var s SessionInfo
		_ = rows.Scan(&s.ID, &s.Created, &s.Agent)
		s.Current = s.ID == cur
		s.ID = s.ID[:12]
		out = append(out, s)
	}
	return out
}

func (a *Auth) EndSession(userID int64, idPrefix string) {
	_, _ = a.c.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND substr(token_hash,1,12)=?`, userID, idPrefix)
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}
