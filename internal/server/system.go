package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
	"github.com/MightyX3N/zeroos/internal/storage"
)

// sysStats samples CPU use in the background.
type sysStats struct {
	mu  sync.Mutex
	cpu float64
}

func newSysStats() *sysStats {
	s := &sysStats{}
	go s.loop()
	return s
}

func readCPU() (idle, total uint64) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	f := strings.Fields(line)
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i == 3 || i == 4 {
			idle += n
		}
	}
	return
}

func (s *sysStats) loop() {
	i1, t1 := readCPU()
	for {
		time.Sleep(3 * time.Second)
		i2, t2 := readCPU()
		if t2 > t1 {
			s.mu.Lock()
			s.cpu = 100 * (1 - float64(i2-i1)/float64(t2-t1))
			s.mu.Unlock()
		}
		i1, t1 = i2, t2
	}
}

func memInfo() (total, avail uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = n * 1024
		case "MemAvailable:":
			avail = n * 1024
		}
	}
	return
}

func uptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.Fields(string(b))[0], 64)
	return int64(f)
}

func (s *sysStats) snapshot(dataDir string) map[string]any {
	s.mu.Lock()
	cpu := s.cpu
	s.mu.Unlock()
	mt, ma := memInfo()
	ssd, _ := storage.UsageOf(dataDir)
	model := ""
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				model = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				break
			}
		}
	}
	return map[string]any{
		"cpu": cpu, "cpuModel": model, "cores": runtime.NumCPU(),
		"memTotal": mt, "memUsed": mt - ma, "ssd": ssd,
		"uptime": uptime(), "hostname": hostname(), "version": core.Version,
		"gpu": dirExists("/dev/dri"),
	}
}

// --- local socket for the command-line tool --------------------------------
//
// Only root can open the socket, and whoever can is treated as the owner.

func (s *Server) serveSocket(ctx context.Context) {
	path := s.C.Cfg.Socket
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return
	}
	_ = os.Chmod(path, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		list, _ := s.Apps.List()
		s.Apps.Status(list)
		shares, _ := s.Store.Shares()
		type a struct{ ID, Name, State, Problem string }
		var out []a
		for _, x := range list {
			out = append(out, a{x.ID, x.Name, x.State, x.Problem})
		}
		ok(w, map[string]any{
			"version": core.Version, "addresses": s.WebAddresses(), "setupDone": s.Auth.SetupDone(),
			"apps": out, "shares": shares, "playit": s.Playit.Info(), "system": s.sys.snapshot(s.C.Cfg.DataDir),
		})
	})
	mux.HandleFunc("POST /apps/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var err error
		switch r.PathValue("action") {
		case "start":
			err = s.Apps.Start(id)
		case "stop":
			err = s.Apps.Stop(id)
		case "restart":
			err = s.Apps.Restart(id)
		default:
			err = errors.New("unknown action")
		}
		s.C.Log("command line", r.PathValue("action"), id, id, err == nil, errText(err))
		if err != nil {
			failErr(w, err)
			return
		}
		ok(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /apps/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.Apps.Logs(r.PathValue("id"), 200)
		if err != nil {
			failErr(w, err)
			return
		}
		ok(w, map[string]string{"log": out})
	})
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	_ = srv.Serve(ln)
}

// SocketGet is used by the command-line tool.
func SocketCall(sock, method, path string, out any) error {
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, _ := http.NewRequest(method, "http://zeroos"+path, nil)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("ZeroOS isn't running (or run this with sudo).")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return errors.New(e.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

var _ = exec.Command
