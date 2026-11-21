package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/MightyX3N/zeroos/internal/auth"
	"github.com/MightyX3N/zeroos/internal/core"
)

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, tok string, remember bool) {
	// Over an encrypted address the cookie is never sent unencrypted.
	c := &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || viaTunnel(r)}
	if remember {
		c.Expires = time.Now().Add(30 * 24 * time.Hour)
	}
	http.SetCookie(w, c)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"setupDone": s.Auth.SetupDone(), "version": core.Version, "hostname": hostname()}
	if c, err := r.Cookie(cookieName); err == nil {
		if u, err := s.Auth.Session(c.Value); err == nil {
			out["user"] = s.meData(u)
		}
	}
	ok(w, out)
}

func (s *Server) setupCode(w http.ResponseWriter, r *http.Request) {
	if s.Auth.SetupDone() {
		fail(w, 400, "This server is already set up. Sign in instead.")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	time.Sleep(400 * time.Millisecond) // slows down guessing
	if !s.Auth.CheckSetupCode(in.Code) {
		fail(w, 400, "That code doesn't match. It's shown in the terminal where ZeroOS was installed, and in /var/lib/zeroos/setup-code.")
		return
	}
	ticket := core.RandomHex(16)
	s.tickets.Store(ticket, time.Now().Add(30*time.Minute))
	ok(w, map[string]string{"ticket": ticket})
}

func (s *Server) setupOwner(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ticket   string `json:"ticket"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	exp, found := s.tickets.Load(in.Ticket)
	if !found || time.Now().After(exp.(time.Time)) {
		fail(w, 400, "Enter the setup code again.")
		return
	}
	id, err := s.Auth.CreateOwner(strings.TrimSpace(in.Username), in.Password)
	if err != nil {
		failErr(w, err)
		return
	}
	s.tickets.Delete(in.Ticket)
	tok, err := s.Auth.NewSession(id, r.UserAgent())
	if err != nil {
		failErr(w, err)
		return
	}
	s.setCookie(w, r, tok, false)
	s.C.Log(in.Username, "created the owner account", "", "", true, "")
	u, _ := s.Auth.UserByID(id)
	ok(w, map[string]any{"user": s.meData(u)})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		fail(w, 403, "Blocked a request from another website.")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
		Remember bool   `json:"remember"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	tok, u, err := s.Auth.Login(strings.TrimSpace(in.Username), in.Password, in.Code, r.UserAgent(), in.Remember)
	if err != nil {
		if err == auth.ErrNeedCode {
			writeJSON(w, 400, map[string]any{"error": err.Error(), "needCode": true})
			return
		}
		s.C.Log(in.Username, "failed sign-in", "", "", false, clientIP(r))
		failErr(w, err)
		return
	}
	s.setCookie(w, r, tok, in.Remember)
	ok(w, map[string]any{"user": s.meData(u)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.Auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) meData(u *auth.User) map[string]any {
	return map[string]any{
		"id": u.ID, "username": u.Username, "role": u.RoleName, "isOwner": u.IsOwner,
		"perms": u.Perms(), "totp": u.TOTP,
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) { ok(w, s.meData(userOf(r))) }

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	u := userOf(r)
	if !s.Auth.VerifyPassword(u.ID, in.Old) {
		fail(w, 400, "Your current password is wrong.")
		return
	}
	if err := s.Auth.SetPassword(u.ID, in.New); err != nil {
		failErr(w, err)
		return
	}
	tok, _ := s.Auth.NewSession(u.ID, r.UserAgent())
	s.setCookie(w, r, tok, false)
	s.log(r, "changed their password", "", "", true, "")
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) mySessions(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie(cookieName)
	ok(w, s.Auth.Sessions(userOf(r).ID, c.Value))
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	s.Auth.EndSession(userOf(r).ID, r.PathValue("sid"))
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) totpBegin(w http.ResponseWriter, r *http.Request) {
	secret, link := auth.NewTOTPSecret(userOf(r).Username)
	ok(w, map[string]string{"secret": secret, "link": link})
}

func (s *Server) totpEnable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Secret string `json:"secret"`
		Code   string `json:"code"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	if err := s.Auth.EnableTOTP(userOf(r).ID, in.Secret, in.Code); err != nil {
		failErr(w, err)
		return
	}
	s.log(r, "turned on two-step sign-in", "", "", true, "")
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request) {
	_ = s.Auth.DisableTOTP(userOf(r).ID)
	s.log(r, "turned off two-step sign-in", "", "", true, "")
	ok(w, map[string]bool{"ok": true})
}

func clientIP(r *http.Request) string {
	h := r.RemoteAddr
	if i := strings.LastIndex(h, ":"); i > 0 {
		h = h[:i]
	}
	return h
}
