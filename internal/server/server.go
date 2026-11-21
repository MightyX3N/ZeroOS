// Package server is ZeroOS's web server: the JSON API and the desktop web UI.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MightyX3N/zeroos/internal/apps"
	"github.com/MightyX3N/zeroos/internal/auth"
	"github.com/MightyX3N/zeroos/internal/core"
	"github.com/MightyX3N/zeroos/internal/playit"
	"github.com/MightyX3N/zeroos/internal/storage"
)

type Server struct {
	C       *core.Core
	Auth    *auth.Auth
	Cat     *apps.Catalog
	Apps    *apps.Manager
	Store   *storage.Storage
	Playit  *playit.Playit
	Web     fs.FS
	sys     *sysStats
	tickets sync.Map // setup tickets after the setup code was entered
}

const cookieName = "zeroos_session"

type ctxKey int

const userKey ctxKey = 1

func New(c *core.Core, a *auth.Auth, cat *apps.Catalog, m *apps.Manager, st *storage.Storage, p *playit.Playit, web fs.FS) *Server {
	return &Server{C: c, Auth: a, Cat: cat, Apps: m, Store: st, Playit: p, Web: web, sys: newSysStats()}
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, v any) { writeJSON(w, 200, v) }

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func failErr(w http.ResponseWriter, err error) {
	fail(w, 400, err.Error())
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("The request wasn't understood.")
	}
	return nil
}

func userOf(r *http.Request) *auth.User {
	u, _ := r.Context().Value(userKey).(*auth.User)
	return u
}

func pathInt(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

// sameOrigin blocks cross-site form posts to the API.
func sameOrigin(r *http.Request) bool {
	if r.Method == "GET" || r.Method == "HEAD" {
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// --- middleware ------------------------------------------------------------

type handler func(w http.ResponseWriter, r *http.Request)

// signedIn requires a session.
func (s *Server) signedIn(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			fail(w, 403, "Blocked a request from another website.")
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil {
			fail(w, 401, "Please sign in.")
			return
		}
		u, err := s.Auth.Session(c.Value)
		if err != nil {
			fail(w, 401, "Please sign in again.")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// need requires a global permission.
func (s *Server) need(perm string, h handler) http.HandlerFunc {
	return s.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if !userOf(r).Can(perm) {
			fail(w, 403, "You don't have permission to do that.")
			return
		}
		h(w, r)
	})
}

// needApp requires a permission on the app in the {id} path segment.
func (s *Server) needApp(perm string, h handler) http.HandlerFunc {
	return s.signedIn(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := s.Apps.Get(id); err != nil {
			fail(w, 404, "App not found.")
			return
		}
		if !s.Auth.CanApp(userOf(r), id, perm) {
			fail(w, 403, "You don't have permission to do that.")
			return
		}
		h(w, r)
	})
}

func (s *Server) log(r *http.Request, action, target, appID string, success bool, detail string) {
	name := "system"
	if u := userOf(r); u != nil {
		name = u.Username
	}
	s.C.Log(name, action, target, appID, success, detail)
}

// --- routes ----------------------------------------------------------------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// before sign-in
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/setup/code", s.setupCode)
	mux.HandleFunc("POST /api/setup/owner", s.setupOwner)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)

	// me
	mux.HandleFunc("GET /api/me", s.signedIn(s.me))
	mux.HandleFunc("PUT /api/me/password", s.signedIn(s.changePassword))
	mux.HandleFunc("GET /api/me/sessions", s.signedIn(s.mySessions))
	mux.HandleFunc("DELETE /api/me/sessions/{sid}", s.signedIn(s.endSession))
	mux.HandleFunc("POST /api/me/totp", s.signedIn(s.totpBegin))
	mux.HandleFunc("PUT /api/me/totp", s.signedIn(s.totpEnable))
	mux.HandleFunc("DELETE /api/me/totp", s.signedIn(s.totpDisable))

	// desktop
	mux.HandleFunc("GET /api/desktop", s.signedIn(s.desktop))
	mux.HandleFunc("GET /api/system", s.need("system.view", s.system))
	mux.HandleFunc("GET /api/activity", s.signedIn(s.activity))

	// store
	mux.HandleFunc("GET /api/store", s.signedIn(s.storeList))
	mux.HandleFunc("GET /api/store/{sid}", s.signedIn(s.storeApp))
	mux.HandleFunc("GET /api/icons/{sid}", s.signedIn(s.icon))
	mux.HandleFunc("POST /api/store/sync", s.need("store.repos", s.storeSync))

	// apps
	mux.HandleFunc("POST /api/apps", s.need("apps.install", s.install))
	mux.HandleFunc("GET /api/jobs/{jid}", s.signedIn(s.job))
	mux.HandleFunc("GET /api/apps/{id}", s.needApp("app.view", s.appDetail))
	mux.HandleFunc("GET /api/apps/{id}/open", s.needApp("app.open", s.appOpen))
	mux.HandleFunc("POST /api/apps/{id}/start", s.needApp("app.power", s.appStart))
	mux.HandleFunc("POST /api/apps/{id}/stop", s.needApp("app.power", s.appStop))
	mux.HandleFunc("POST /api/apps/{id}/restart", s.needApp("app.power", s.appRestart))
	mux.HandleFunc("GET /api/apps/{id}/logs", s.needApp("app.settings", s.appLogs))
	mux.HandleFunc("PUT /api/apps/{id}/folders", s.needApp("app.settings", s.appFolder))
	mux.HandleFunc("PUT /api/apps/{id}/gpu", s.needApp("app.settings", s.appGPU))
	mux.HandleFunc("PUT /api/apps/{id}/share", s.needApp("app.share", s.appShare))
	mux.HandleFunc("POST /api/apps/{id}/update", s.needApp("app.update", s.appUpdate))
	mux.HandleFunc("POST /api/apps/{id}/check", s.needApp("app.update", s.appCheck))
	mux.HandleFunc("POST /api/apps/{id}/undo", s.needApp("app.update", s.appUndo))
	mux.HandleFunc("DELETE /api/apps/{id}/undo", s.needApp("app.update", s.appDropUndo))
	mux.HandleFunc("GET /api/apps/{id}/access", s.needApp("app.access", s.appAccess))
	mux.HandleFunc("PUT /api/apps/{id}/access", s.needApp("app.access", s.appSetAccess))
	mux.HandleFunc("DELETE /api/apps/{id}", s.needApp("app.remove", s.appRemove))

	// storage
	mux.HandleFunc("GET /api/storage", s.signedIn(s.storageInfo))
	mux.HandleFunc("POST /api/storage/discover", s.need("storage.manage", s.discover))
	mux.HandleFunc("POST /api/storage/list", s.need("storage.manage", s.listShares))
	mux.HandleFunc("POST /api/storage/shares", s.need("storage.manage", s.addShare))
	mux.HandleFunc("DELETE /api/storage/shares/{sid}", s.need("storage.manage", s.removeShare))
	mux.HandleFunc("POST /api/storage/shares/{sid}/reconnect", s.need("storage.manage", s.reconnectShare))
	mux.HandleFunc("PUT /api/storage/shares/{sid}/login", s.need("storage.manage", s.shareLogin))
	mux.HandleFunc("GET /api/folders/roots", s.signedIn(s.folderRoots))
	mux.HandleFunc("GET /api/folders", s.signedIn(s.folders))
	mux.HandleFunc("POST /api/folders", s.signedIn(s.makeFolder))

	// sharing (playit)
	mux.HandleFunc("GET /api/sharing", s.signedIn(s.sharing))
	mux.HandleFunc("POST /api/sharing/claim", s.need("playit.account", s.claimStart))
	mux.HandleFunc("GET /api/sharing/claim", s.need("playit.account", s.claimPoll))
	mux.HandleFunc("POST /api/sharing/key", s.need("playit.account", s.claimKey))
	mux.HandleFunc("DELETE /api/sharing", s.need("playit.account", s.unlink))
	mux.HandleFunc("PUT /api/sharing/remote", s.need("system.manage", s.setRemote))
	mux.HandleFunc("POST /api/sharing/refresh", s.signedIn(s.sharingRefresh))
	mux.HandleFunc("POST /api/sharing/ports", s.signedIn(s.sharePort))
	mux.HandleFunc("DELETE /api/sharing/tunnels/{tid}", s.signedIn(s.removeTunnel))

	// users & roles
	mux.HandleFunc("GET /api/perms", s.signedIn(s.permList))
	mux.HandleFunc("GET /api/users", s.need("users.manage", s.users))
	mux.HandleFunc("POST /api/users", s.need("users.manage", s.createUser))
	mux.HandleFunc("PUT /api/users/{uid}", s.need("users.manage", s.updateUser))
	mux.HandleFunc("DELETE /api/users/{uid}", s.need("users.manage", s.deleteUser))
	mux.HandleFunc("PUT /api/users/{uid}/password", s.need("users.manage", s.resetUserPassword))
	mux.HandleFunc("GET /api/users/{uid}/apps", s.need("users.manage", s.userApps))
	mux.HandleFunc("PUT /api/users/{uid}/apps", s.need("users.manage", s.setUserApps))
	mux.HandleFunc("GET /api/roles", s.signedIn(s.roles))
	mux.HandleFunc("POST /api/roles", s.need("roles.manage", s.saveRole))
	mux.HandleFunc("PUT /api/roles/{rid}", s.need("roles.manage", s.saveRole))
	mux.HandleFunc("DELETE /api/roles/{rid}", s.need("roles.manage", s.deleteRole))

	// web UI
	static := http.FileServer(http.FS(s.Web))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fail(w, 404, "Not found.")
			return
		}
		if r.URL.Path != "/" {
			if _, err := fs.Stat(s.Web, strings.TrimPrefix(r.URL.Path, "/")); err == nil {
				w.Header().Set("Cache-Control", "no-cache")
				static.ServeHTTP(w, r)
				return
			}
		}
		b, _ := fs.ReadFile(s.Web, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
	return securityHeaders(s.guardRemote(mux))
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		h.ServeHTTP(w, r)
	})
}

// Run serves the web UI until ctx ends, and keeps background jobs going.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.C.Cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	go s.background(ctx)
	go s.serveSocket(ctx)
	// The encrypted port, for reaching ZeroOS from outside through playit.
	if cert, err := s.certificate(); err != nil {
		log.Printf("encrypted port not started: %v", err)
	} else if tln, err := tls.Listen("tcp", s.C.Cfg.TLSListen, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}); err != nil {
		log.Printf("encrypted port not started: %v", err)
	} else {
		tsrv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
		go func() { <-ctx.Done(); _ = tsrv.Close() }()
		go func() { _ = tsrv.Serve(tln) }()
	}
	ln, err := net.Listen("tcp", s.C.Cfg.Listen)
	if err != nil {
		return err
	}
	log.Printf("ZeroOS %s is running. Open %s in a browser.", core.Version, s.WebAddresses()[0])
	if !s.Auth.SetupDone() {
		log.Printf("Setup code: %s", s.Auth.SetupCode())
	}
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// background: NAS reconnects, keeping apps in step with the NAS, store sync, tunnel addresses.
func (s *Server) background(ctx context.Context) {
	s.Apps.Recover()
	go s.Store.KeepConnected(ctx, s.Apps.Reconcile)
	go func() {
		s.Playit.EnsureAgent()
		s.Playit.Refresh()
	}()
	go func() {
		// Keep trying until the store has its catalog (the network may not be up yet at boot).
		for wait := 5 * time.Second; !s.Cat.Downloaded(); {
			err := s.Cat.Sync()
			if err == nil {
				break
			}
			log.Printf("store: %v (trying again in %s)", err, wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if wait < 10*time.Minute {
				wait *= 2
			}
		}
	}()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n++
			s.Apps.Reconcile()
			if n%4 == 0 {
				s.Playit.Refresh()
				s.refreshTunnelAddresses()
			}
			if n%(4*60) == 0 {
				s.Apps.ExpireUndo()
			}
			if n == 8 || n%(4*60*24) == 0 {
				go s.Apps.CheckBuilds() // two minutes after starting, then daily
			}
			if n%(4*60*24) == 0 {
				_ = s.Cat.Sync()
			}
		}
	}
}

// WebAddresses lists how to reach the web UI.
func (s *Server) WebAddresses() []string {
	_, port, _ := net.SplitHostPort(s.C.Cfg.Listen)
	suffix := ""
	if port != "80" && port != "" {
		suffix = ":" + port
	}
	host, _ := os.Hostname()
	out := []string{}
	if host != "" {
		out = append(out, "http://"+host+".local"+suffix)
	}
	out = append(out, "http://"+apps.HostIP()+suffix)
	return out
}
