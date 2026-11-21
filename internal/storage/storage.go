// Package storage handles the SSD space, NAS shares (SMB) and the folder picker.
package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
	"github.com/hirochachacha/go-smb2"
)

// AppUID/AppGID own files on NAS mounts so app containers (which mostly run as 1000) can write.
const AppUID, AppGID = 1000, 1000

type Storage struct {
	c  *core.Core
	mu sync.Mutex
	// remember mount problems so the UI can explain them
	lastErr map[int64]string
	// mountCmd lets tests replace the real mount command.
	mountCmd func(args ...string) ([]byte, error)
}

func New(c *core.Core) *Storage {
	return &Storage{c: c, lastErr: map[int64]string{}, mountCmd: func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "mount", args...).CombinedOutput()
	}}
}

// --- space -----------------------------------------------------------------

type Usage struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
	Used  uint64 `json:"used"`
}

// UsageOf reads disk space with a timeout, so a dead NAS can't hang the UI.
func UsageOf(path string) (Usage, error) {
	type res struct {
		u   Usage
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err != nil {
			ch <- res{err: err}
			return
		}
		total := st.Blocks * uint64(st.Bsize)
		free := st.Bavail * uint64(st.Bsize)
		ch <- res{u: Usage{Total: total, Free: free, Used: total - st.Bfree*uint64(st.Bsize)}}
	}()
	select {
	case r := <-ch:
		return r.u, r.err
	case <-time.After(3 * time.Second):
		return Usage{}, errors.New("timed out")
	}
}

// --- NAS shares ------------------------------------------------------------

type Share struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Host       string   `json:"host"`
	Share      string   `json:"share"`
	Username   string   `json:"username"`
	MountPoint string   `json:"mountPoint"`
	Connected  bool     `json:"connected"`
	Problem    string   `json:"problem,omitempty"`
	Usage      *Usage   `json:"usage,omitempty"`
	UsedBy     []string `json:"usedBy"`
}

var safeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func (s *Storage) mountPoint(name string) string {
	return filepath.Join(s.c.Cfg.MountRoot, safeName.ReplaceAllString(name, "-"))
}

func (s *Storage) Shares() ([]*Share, error) {
	rows, err := s.c.DB.Query(`SELECT id,name,host,share,username FROM nas_shares ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Share
	for rows.Next() {
		sh := &Share{UsedBy: []string{}}
		if err := rows.Scan(&sh.ID, &sh.Name, &sh.Host, &sh.Share, &sh.Username); err != nil {
			return nil, err
		}
		sh.MountPoint = s.mountPoint(sh.Name)
		out = append(out, sh)
	}
	for _, sh := range out {
		sh.Connected = s.Healthy(sh.MountPoint)
		if sh.Connected {
			if u, err := UsageOf(sh.MountPoint); err == nil {
				sh.Usage = &u
			}
		} else {
			s.mu.Lock()
			sh.Problem = s.lastErr[sh.ID]
			s.mu.Unlock()
		}
	}
	return out, nil
}

func (s *Storage) ShareByID(id int64) (*Share, error) {
	all, err := s.Shares()
	if err != nil {
		return nil, err
	}
	for _, sh := range all {
		if sh.ID == id {
			return sh, nil
		}
	}
	return nil, errors.New("NAS share not found")
}

// IsMounted checks /proc/mounts.
func IsMounted(path string) bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && unescapeMount(f[1]) == path {
			return true
		}
	}
	return false
}

func unescapeMount(s string) string { return strings.ReplaceAll(s, `\040`, " ") }

// Healthy means mounted and answering within a few seconds.
func (s *Storage) Healthy(mp string) bool {
	if !IsMounted(mp) {
		return false
	}
	ch := make(chan bool, 1)
	go func() {
		_, err := os.ReadDir(mp)
		ch <- err == nil
	}()
	select {
	case ok := <-ch:
		return ok
	case <-time.After(4 * time.Second):
		return false
	}
}

func (s *Storage) credsFile(id int64) string {
	return filepath.Join(s.c.Cfg.DataDir, "creds", fmt.Sprintf("nas-%d", id))
}

// mount mounts one share with the kernel SMB client.
func (s *Storage) mount(id int64, host, share, user, pass, mp string) error {
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return err
	}
	if IsMounted(mp) {
		return nil
	}
	cf := s.credsFile(id)
	if err := os.MkdirAll(filepath.Dir(cf), 0o700); err != nil {
		return err
	}
	creds := fmt.Sprintf("username=%s\npassword=%s\n", user, pass)
	if user == "" {
		creds = "username=guest\npassword=\n"
	}
	if err := os.WriteFile(cf, []byte(creds), 0o600); err != nil {
		return err
	}
	opts := fmt.Sprintf("credentials=%s,uid=%d,gid=%d,file_mode=0775,dir_mode=0775,iocharset=utf8,soft,echo_interval=10", cf, AppUID, AppGID)
	out, err := s.mountCmd("-t", "cifs", "//"+host+"/"+share, mp, "-o", opts)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		switch {
		case strings.Contains(msg, "Permission denied") || strings.Contains(msg, "error(13)"):
			return errors.New("The NAS refused the username or password.")
		case strings.Contains(msg, "No such file") || strings.Contains(msg, "error(2)"):
			return errors.New("That share doesn't exist on the NAS.")
		case strings.Contains(msg, "Host is down") || strings.Contains(msg, "error(112)") || strings.Contains(msg, "timed out"):
			return errors.New("The NAS isn't answering.")
		case strings.Contains(msg, "wrong fs type") || strings.Contains(msg, "unknown filesystem") || strings.Contains(msg, "error(19)"):
			return errors.New("This computer is missing the SMB tools (cifs-utils). Run the ZeroOS installer again.")
		}
		if msg == "" {
			msg = err.Error()
		}
		return errors.New("Couldn't connect the share: " + msg)
	}
	return nil
}

// AddShare tests, saves and mounts a NAS share.
func (s *Storage) AddShare(name, host, share, user, pass string) (*Share, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = share
	}
	if host == "" || share == "" {
		return nil, errors.New("Pick a NAS and a share.")
	}
	var n int
	_ = s.c.DB.QueryRow(`SELECT COUNT(*) FROM nas_shares WHERE name=?`, name).Scan(&n)
	if n > 0 {
		return nil, errors.New("A share with that name is already added.")
	}
	enc, err := s.c.Encrypt(pass)
	if err != nil {
		return nil, err
	}
	res, err := s.c.DB.Exec(`INSERT INTO nas_shares(name,host,share,username,password_enc,created) VALUES(?,?,?,?,?,?)`, name, host, share, user, enc, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	mp := s.mountPoint(name)
	if err := s.mount(id, host, share, user, pass, mp); err != nil {
		_, _ = s.c.DB.Exec(`DELETE FROM nas_shares WHERE id=?`, id)
		_ = os.Remove(s.credsFile(id))
		return nil, err
	}
	// Prove we can write, as apps will need to.
	probe := filepath.Join(mp, ".zeroos-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		_ = exec.Command("umount", "-l", mp).Run()
		_, _ = s.c.DB.Exec(`DELETE FROM nas_shares WHERE id=?`, id)
		return nil, errors.New("Connected, but this account can't write to the share. Give it write access on the NAS.")
	}
	_ = os.Remove(probe)
	return s.ShareByID(id)
}

// RemoveShare unmounts and forgets a share. Apps using it must be removed first.
func (s *Storage) RemoveShare(id int64) error {
	sh, err := s.ShareByID(id)
	if err != nil {
		return err
	}
	_ = exec.Command("umount", "-l", sh.MountPoint).Run()
	_ = os.Remove(sh.MountPoint)
	_ = os.Remove(s.credsFile(id))
	_, err = s.c.DB.Exec(`DELETE FROM nas_shares WHERE id=?`, id)
	return err
}

// Reconnect tries to mount one share now.
func (s *Storage) Reconnect(id int64) error {
	var name, host, share, user, enc string
	if err := s.c.DB.QueryRow(`SELECT name,host,share,username,password_enc FROM nas_shares WHERE id=?`, id).Scan(&name, &host, &share, &user, &enc); err != nil {
		return errors.New("NAS share not found")
	}
	pass, _ := s.c.Decrypt(enc)
	mp := s.mountPoint(name)
	if IsMounted(mp) && !s.Healthy(mp) {
		_ = exec.Command("umount", "-l", mp).Run()
	}
	err := s.mount(id, host, share, user, pass, mp)
	s.mu.Lock()
	if err != nil {
		s.lastErr[id] = err.Error()
	} else {
		delete(s.lastErr, id)
	}
	s.mu.Unlock()
	return err
}

// UpdateLogin changes the username/password of a share and reconnects.
func (s *Storage) UpdateLogin(id int64, user, pass string) error {
	enc, err := s.c.Encrypt(pass)
	if err != nil {
		return err
	}
	if _, err := s.c.DB.Exec(`UPDATE nas_shares SET username=?, password_enc=? WHERE id=?`, user, enc, id); err != nil {
		return err
	}
	return s.Reconnect(id)
}

// KeepConnected reconnects lost shares every 30 seconds until ctx ends.
func (s *Storage) KeepConnected(ctx context.Context, onChange func()) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	check := func() {
		shares, _ := s.Shares()
		changed := false
		for _, sh := range shares {
			if !sh.Connected {
				if s.Reconnect(sh.ID) == nil {
					changed = true
				}
			}
		}
		if changed && onChange != nil {
			onChange()
		}
	}
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// --- discovery -------------------------------------------------------------

type FoundNAS struct {
	Host string `json:"host"`
	Name string `json:"name"`
}

// Discover looks for SMB servers (port 445) on the local networks.
func Discover(ctx context.Context) []FoundNAS {
	var targets []net.IP
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "br-") || strings.HasPrefix(ifc.Name, "veth") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ones, _ := ipn.Mask.Size()
			if ones < 22 { // only scan normal home-sized networks
				ones = 24
			}
			base := ipn.IP.To4().Mask(net.CIDRMask(ones, 32))
			count := 1 << (32 - ones)
			for i := 1; i < count-1 && i < 1024; i++ {
				ip := make(net.IP, 4)
				copy(ip, base)
				v := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
				v += uint32(i)
				ip[0], ip[1], ip[2], ip[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
				if !ip.Equal(ipn.IP) {
					targets = append(targets, ip)
				}
			}
		}
	}
	var mu sync.Mutex
	found := []FoundNAS{}
	sem := make(chan struct{}, 128)
	var wg sync.WaitGroup
	for _, ip := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip net.IP) {
			defer wg.Done()
			defer func() { <-sem }()
			d := net.Dialer{Timeout: 400 * time.Millisecond}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "445"))
			if err != nil {
				return
			}
			conn.Close()
			name := ip.String()
			if names, err := net.DefaultResolver.LookupAddr(ctx, ip.String()); err == nil && len(names) > 0 {
				name = strings.TrimSuffix(strings.Split(names[0], ".")[0], ".")
			}
			mu.Lock()
			found = append(found, FoundNAS{Host: ip.String(), Name: name})
			mu.Unlock()
		}(ip)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool { return found[i].Host < found[j].Host })
	return found
}

// ListShares signs in to an SMB server and lists its shares.
func ListShares(host, user, pass string) ([]string, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "445"), 5*time.Second)
	if err != nil {
		return nil, errors.New("The NAS isn't answering on the network.")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if user == "" {
		user = "guest"
	}
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: pass}}
	sess, err := d.Dial(conn)
	if err != nil {
		if e := strings.ToLower(err.Error()); strings.Contains(e, "logon") || strings.Contains(e, "access_denied") || strings.Contains(e, "access denied") || strings.Contains(e, "password") {
			return nil, errors.New("The NAS refused the username or password.")
		}
		return nil, errors.New("Couldn't sign in to the NAS: " + err.Error())
	}
	defer sess.Logoff()
	names, err := sess.ListSharenames()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if strings.HasSuffix(n, "$") {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// --- folder picker ---------------------------------------------------------

type Root struct {
	ID        string `json:"id"` // "local" or "nas:<id>"
	Label     string `json:"label"`
	Path      string `json:"-"`
	Connected bool   `json:"connected"`
}

// Roots are the places a user may pick folders from.
func (s *Storage) Roots() []Root {
	roots := []Root{{ID: "local", Label: "This mini PC", Path: s.c.Cfg.MediaDir(), Connected: true}}
	shares, _ := s.Shares()
	for _, sh := range shares {
		roots = append(roots, Root{ID: fmt.Sprintf("nas:%d", sh.ID), Label: sh.Name + " (NAS)", Path: sh.MountPoint, Connected: sh.Connected})
	}
	return roots
}

// Resolve turns a picked folder into a host path, refusing anything that escapes its root.
func (s *Storage) Resolve(rootID, rel string) (string, error) {
	for _, r := range s.Roots() {
		if r.ID == rootID {
			clean := filepath.Clean("/" + rel)
			return filepath.Join(r.Path, clean), nil
		}
	}
	return "", errors.New("That place isn't available any more.")
}

// RootConnected tells whether a root is usable right now.
func (s *Storage) RootConnected(rootID string) bool {
	for _, r := range s.Roots() {
		if r.ID == rootID {
			return r.Connected
		}
	}
	return false
}

type Folder struct {
	Name  string `json:"name"`
	Items int    `json:"items"`
}

// Browse lists sub-folders of a picked place.
func (s *Storage) Browse(rootID, rel string) ([]Folder, error) {
	p, err := s.Resolve(rootID, rel)
	if err != nil {
		return nil, err
	}
	if !s.RootConnected(rootID) {
		return nil, errors.New("This NAS share isn't connected right now.")
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, errors.New("Couldn't open that folder.")
	}
	out := []Folder{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "@") || e.Name() == "#recycle" {
			continue
		}
		n := 0
		if sub, err := os.ReadDir(filepath.Join(p, e.Name())); err == nil {
			n = len(sub)
		}
		out = append(out, Folder{Name: e.Name(), Items: n})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// MakeFolder creates a new folder inside a picked place.
func (s *Storage) MakeFolder(rootID, rel, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return errors.New("Pick a simple folder name.")
	}
	p, err := s.Resolve(rootID, filepath.Join(rel, name))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p, 0o775); err != nil {
		return errors.New("Couldn't create the folder.")
	}
	_ = os.Chown(p, AppUID, AppGID)
	return nil
}
