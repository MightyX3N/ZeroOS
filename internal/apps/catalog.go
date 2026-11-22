// Package apps fills the store from existing catalogs and runs apps with Docker Compose.
package apps

import (
	"archive/tar"
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
)

// RuntipiURL is the Runtipi app store (GPL-3.0), read as a GitHub tarball.
// HEAD follows the catalog's default branch, whatever it is called.
var RuntipiURL = "https://codeload.github.com/runtipi/runtipi-appstore/tar.gz/HEAD"

// builtin holds the apps ZeroOS brings itself, in the same format as the
// catalog. They are for apps the catalog lacks or that need setting up after
// they start (a setup.sh next to the app), so they work right after install.
//
//go:embed all:builtin
var builtinFS embed.FS

//go:embed verified.json
var verifiedJSON []byte

// overrides add friendlier words to imported apps without changing how they run.
//
//go:embed overrides.json
var overridesJSON []byte

type FormField struct {
	Type        string `json:"type"`
	Label       string `json:"label"`
	Hint        string `json:"hint,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Default     any    `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	EnvVariable string `json:"env_variable"`
	Encoding    string `json:"encoding,omitempty"` // for "random": base64 or hex of that many bytes
	Min         *int   `json:"min,omitempty"`
	Max         *int   `json:"max,omitempty"`
	Options     []struct {
		Label string `json:"label"`
		Value any    `json:"value"`
	} `json:"options,omitempty"`
}

// StoreApp is one app in the store.
type StoreApp struct {
	ID          string       `json:"id"`
	Catalog     string       `json:"catalog"`
	Name        string       `json:"name"`
	ShortDesc   string       `json:"shortDesc"`
	Description string       `json:"description"`
	Categories  []string     `json:"categories"`
	Version     string       `json:"version"`
	Website     string       `json:"website,omitempty"`
	Source      string       `json:"source,omitempty"`
	Verified    bool         `json:"verified"`
	NoGUI       bool         `json:"noGui"`
	URLSuffix   string       `json:"urlSuffix,omitempty"`
	HTTPS       bool         `json:"https,omitempty"`
	Form        []FormField  `json:"form"`
	Folders     []FolderNeed `json:"folders"`
	UsesDocker  bool         `json:"usesDocker"`
	SharePort   int          `json:"-"`
	SharingNote string       `json:"-"`
	dir         string
	images      []string // what it runs, for telling the same app in another catalog
	fixedPort   int      // its page is on this port, whatever ZeroOS would pick
}

// FolderNeed is a big-files folder the app wants (from the NAS, usually).
type FolderNeed struct {
	Key   string `json:"key"`   // the host path template in the catalog
	Label string `json:"label"` // shown in the install wizard
	Hint  string `json:"hint"`
}

type Catalog struct {
	c        *core.Core
	mu       sync.RWMutex
	apps     map[string]*StoreApp
	verified map[string]bool
	over     map[string]override
	syncing  bool
}

type override struct {
	Folders map[string]FolderNeed `json:"folders"`
	Name    string                `json:"name"`
}

func NewCatalog(c *core.Core) *Catalog {
	cat := &Catalog{c: c, apps: map[string]*StoreApp{}, verified: map[string]bool{}, over: map[string]override{}}
	_ = json.Unmarshal(overridesJSON, &cat.over)
	cat.unpackBuiltin()
	_ = cat.Load()
	return cat
}

func (cat *Catalog) runtipiDir() string { return filepath.Join(cat.c.Cfg.StoreDir(), "runtipi") }
func (cat *Catalog) builtinDir() string { return filepath.Join(cat.c.Cfg.StoreDir(), "zeroos") }

// unpackBuiltin writes ZeroOS's own apps to disk, fresh on every start.
func (cat *Catalog) unpackBuiltin() {
	dest := cat.builtinDir()
	_ = os.RemoveAll(dest)
	_ = fs.WalkDir(builtinFS, "builtin", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		target := filepath.Join(dest, strings.TrimPrefix(p, "builtin"))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := builtinFS.ReadFile(p)
		if err != nil {
			return nil
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// Hook returns a script that belongs to one of ZeroOS's own apps ("setup",
// "stop" or "remove"). It is read from inside the program, never from a
// catalog that was downloaded.
func (cat *Catalog) Hook(a *StoreApp, name string) []byte {
	if a == nil || a.Catalog != "zeroos" {
		return nil
	}
	b, err := builtinFS.ReadFile("builtin/" + a.ID + "/" + name + ".sh")
	if err != nil {
		return nil
	}
	return b
}

// Sync downloads the newest catalogs. Safe to call while running. One catalog
// failing doesn't hold back the other.
func (cat *Catalog) Sync() error {
	cat.mu.Lock()
	if cat.syncing {
		cat.mu.Unlock()
		return nil
	}
	cat.syncing = true
	cat.mu.Unlock()
	defer func() { cat.mu.Lock(); cat.syncing = false; cat.mu.Unlock() }()
	err := cat.syncRuntipi()
	if err2 := cat.syncCasa(); err2 != nil {
		log.Printf("store: %v", err2)
	}
	if err2 := cat.syncVerified(); err2 != nil {
		log.Printf("store: %v", err2)
	}
	_ = cat.Load()
	return err
}

func (cat *Catalog) syncRuntipi() error {

	src := RuntipiURL
	if v := os.Getenv("ZEROOS_STORE_URL"); v != "" {
		src = v // for offline testing: a file:// path to the same tarball
	}
	var body io.Reader
	if strings.HasPrefix(src, "file://") {
		f, err := os.Open(strings.TrimPrefix(src, "file://"))
		if err != nil {
			return err
		}
		defer f.Close()
		body = f
	} else {
		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Get(src)
		if err != nil {
			return fmt.Errorf("couldn't download the app catalog: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("couldn't download the app catalog (HTTP %d)", resp.StatusCode)
		}
		body = resp.Body
	}
	tmp := cat.runtipiDir() + ".new"
	_ = os.RemoveAll(tmp)
	if err := extractApps(body, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	old := cat.runtipiDir() + ".old"
	_ = os.RemoveAll(old)
	_ = os.Rename(cat.runtipiDir(), old)
	if err := os.Rename(tmp, cat.runtipiDir()); err != nil {
		_ = os.Rename(old, cat.runtipiDir())
		return err
	}
	_ = os.RemoveAll(old)
	_ = cat.c.SetSetting("store_synced", fmt.Sprint(time.Now().Unix()))
	return nil
}

// extractApps keeps only apps/<id>/... from the GitHub tarball.
func extractApps(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		parts := strings.SplitN(h.Name, "/", 2)
		if len(parts) < 2 || !strings.HasPrefix(parts[1], "apps/") {
			continue
		}
		rel := filepath.Clean(strings.TrimPrefix(parts[1], "apps/"))
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		target := filepath.Join(dest, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			if h.Size > 20<<20 {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			n++
		}
	}
	if n == 0 {
		return errors.New("the app catalog download was empty")
	}
	return nil
}

type runtipiConfig struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Available     bool        `json:"available"`
	Deprecated    bool        `json:"deprecated"`
	ShortDesc     string      `json:"short_desc"`
	Description   string      `json:"description"`
	Categories    []string    `json:"categories"`
	Version       string      `json:"version"`
	Website       string      `json:"website"`
	Source        string      `json:"source"`
	NoGUI         bool        `json:"no_gui"`
	URLSuffix     string      `json:"url_suffix"`
	HTTPS         bool        `json:"https"`
	Port          int         `json:"port"`
	FormFields    []FormField `json:"form_fields"`
	Architectures []string    `json:"supported_architectures"`
	// ZeroOS's own additions, for its own apps:
	SharePort   int                   `json:"share_port"`   // the port for visitors from outside, when the app has a separate one
	SharingNote string                `json:"sharing_note"` // shown with the app's Share online switch
	Folders     map[string]FolderNeed `json:"folders"`      // friendlier words for the folders it asks for
}

// Load reads the catalogs from disk: the downloaded one, then ZeroOS's own
// apps, which win when both have the same app.
func (cat *Catalog) Load() error {
	apps := map[string]*StoreApp{}
	cat.verified = cat.loadVerified()
	cat.loadDir(apps, cat.runtipiDir(), "runtipi")
	cat.loadDir(apps, cat.builtinDir(), "zeroos")
	cat.loadCasa(apps) // only what the others lack
	cat.mu.Lock()
	cat.apps = apps
	cat.mu.Unlock()
	return nil
}

func (cat *Catalog) loadDir(apps map[string]*StoreApp, root, catalog string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return // not there yet
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			continue
		}
		var rc runtipiConfig
		if json.Unmarshal(b, &rc) != nil || !rc.Available || rc.Deprecated || rc.ID == "" {
			continue
		}
		if len(rc.Architectures) > 0 && !contains(rc.Architectures, runtime.GOARCH) {
			continue
		}
		desc := rc.Description
		if md, err := os.ReadFile(filepath.Join(dir, "metadata", "description.md")); err == nil && len(md) > 0 {
			desc = string(md)
		}
		a := &StoreApp{
			ID: rc.ID, Catalog: catalog, Name: rc.Name, ShortDesc: rc.ShortDesc, Description: desc,
			Categories: rc.Categories, Version: rc.Version, Website: rc.Website, Source: rc.Source,
			NoGUI: rc.NoGUI, URLSuffix: rc.URLSuffix, HTTPS: rc.HTTPS, Form: rc.FormFields, dir: dir,
			// ZeroOS's own apps are made for it and tested with it.
			Verified: cat.verified[rc.ID] || catalog == "zeroos",
		}
		if a.Form == nil {
			a.Form = []FormField{}
		}
		plan, err := loadCompose(dir)
		if err != nil {
			continue // a format we can't run yet
		}
		a.Folders = cat.folderNeeds(a.ID, plan)
		for i, f := range a.Folders {
			if w, ok := rc.Folders[f.Key]; ok {
				if w.Label != "" {
					a.Folders[i].Label = w.Label
				}
				if w.Hint != "" {
					a.Folders[i].Hint = w.Hint
				}
			}
		}
		if catalog == "zeroos" {
			a.SharePort, a.SharingNote = rc.SharePort, rc.SharingNote
		}
		a.UsesDocker = plan.usesDockerSocket()
		a.images = plan.imageRepos()
		// An app on the computer's own network listens where it wants to, not where ZeroOS would put it.
		if host, port := plan.mainOnHost(); host && !rc.NoGUI {
			if a.fixedPort = port; port == 0 {
				a.fixedPort = rc.Port
			}
		}
		if o, ok := cat.over[a.ID]; ok && o.Name != "" {
			a.Name = o.Name
		}
		apps[a.ID] = a
	}
}

func (cat *Catalog) folderNeeds(id string, p *composePlan) []FolderNeed {
	seen := map[string]bool{}
	out := []FolderNeed{}
	for _, hp := range p.hostPaths() {
		if !strings.HasPrefix(hp, "${ROOT_FOLDER_HOST}") || seen[hp] {
			continue
		}
		seen[hp] = true
		need := FolderNeed{Key: hp, Label: friendlyFolder(hp), Hint: "Pick a folder on your NAS. The app sees it as a normal folder."}
		if o, ok := cat.over[id]; ok {
			if f, ok := o.Folders[hp]; ok {
				if f.Label != "" {
					need.Label = f.Label
				}
				if f.Hint != "" {
					need.Hint = f.Hint
				}
			}
		}
		out = append(out, need)
	}
	return out
}

func friendlyFolder(hp string) string {
	p := strings.Trim(strings.TrimPrefix(hp, "${ROOT_FOLDER_HOST}"), "/")
	segs := strings.Split(p, "/")
	last := segs[len(segs)-1]
	switch strings.ToLower(last) {
	case "", "media":
		return "Media folder"
	case "data":
		return "Media library"
	case "torrents", "downloads", "complete", "incomplete":
		return "Downloads folder"
	}
	return strings.ToUpper(last[:1]) + last[1:] + " folder"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// List returns store apps, filtered by a search text and category.
func (cat *Catalog) List(q, category string, verifiedOnly bool) []*StoreApp {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	q = strings.ToLower(strings.TrimSpace(q))
	var out []*StoreApp
	for _, a := range cat.apps {
		if verifiedOnly && !a.Verified {
			continue
		}
		if category != "" && !contains(a.Categories, category) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(a.Name+" "+a.ShortDesc+" "+strings.Join(a.Categories, " ")), q) {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Verified != out[j].Verified {
			return out[i].Verified
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (cat *Catalog) Get(id string) (*StoreApp, bool) {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	a, ok := cat.apps[id]
	return a, ok
}

func (cat *Catalog) Count() int {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	return len(cat.apps)
}

// Downloaded tells whether the big catalog has arrived (ZeroOS's own apps are always there).
func (cat *Catalog) Downloaded() bool {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	for _, a := range cat.apps {
		if a.Catalog == "runtipi" {
			return true
		}
	}
	return false
}

func (cat *Catalog) Categories() []string {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	set := map[string]bool{}
	for _, a := range cat.apps {
		for _, c := range a.Categories {
			set[c] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// LogoPath is the app's icon on disk, if any.
func (cat *Catalog) LogoPath(id string) string {
	a, ok := cat.Get(id)
	if !ok {
		return ""
	}
	if a.Catalog == "casaos" {
		return cat.casaIcon(id)
	}
	p := filepath.Join(a.dir, "metadata", "logo.jpg")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
