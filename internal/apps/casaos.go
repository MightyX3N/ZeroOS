package apps

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// The CasaOS / ZimaOS app store (Apache-2.0) as a second source for the Store.
//
// Its apps are plain compose files with an "x-casaos" part that describes
// them. Each is turned into the same kind of recipe as an app of your own: the
// page moves to a port ZeroOS picks, the app's own folders (/DATA/AppData/…)
// land in its folder on the SSD, and other folders (/DATA/Media, …) are asked
// for with the folder picker. An app the other catalogs already have is left
// out, so nothing shows up twice.

// CasaOSURL is the store as CasaOS itself downloads it: only the compose files.
var CasaOSURL = "https://raw.githubusercontent.com/IceWhaleTech/CasaOS-AppStore/gh-pages/store/main.zip"

func (cat *Catalog) casaDir() string     { return filepath.Join(cat.c.Cfg.StoreDir(), "casaos") }
func (cat *Catalog) casaIconDir() string { return filepath.Join(cat.c.Cfg.StoreDir(), "casaos-icons") }

// casaConfig is the config.json written for a converted app. It is read back
// both as a catalog entry and as a custom-style recipe.
type casaConfig struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Available   bool                  `json:"available"`
	ShortDesc   string                `json:"short_desc"`
	Description string                `json:"description"`
	Categories  []string              `json:"categories"`
	Version     string                `json:"version"`
	Website     string                `json:"website,omitempty"`
	Source      string                `json:"source,omitempty"`
	NoGUI       bool                  `json:"no_gui"`
	URLSuffix   string                `json:"url_suffix,omitempty"`
	FormFields  []FormField           `json:"form_fields"`
	Folders     map[string]FolderNeed `json:"folders"`
	Custom      customInfo            `json:"custom"`
	Icon        string                `json:"icon,omitempty"`
	Images      []string              `json:"images"` // for telling the same app in another catalog
}

var casaCategories = map[string]string{"media": "media", "productivity": "utilities", "ai": "ai", "networking": "network", "network": "network",
	"developer": "development", "home": "automation", "home automation": "automation", "finance": "finance", "social": "social", "games": "gaming",
	"gaming": "gaming", "utilities": "utilities", "backup": "data", "cloud": "data", "downloader": "media", "security": "security", "photos": "photography"}

// casaText picks the English words out of a field that is written per language.
func casaText(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		for _, k := range []string{"en_us", "en_US", "en_GB", "en_gb", "en"} {
			if s, ok := x[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func casaFolderLabel(key string) string {
	base := path.Base(key)
	switch key {
	case "/DATA":
		return "Files folder"
	case "/DATA/Media":
		return "Media folder"
	}
	if base == "" || base == "/" || base == "." {
		return "Folder"
	}
	return strings.ToUpper(base[:1]) + base[1:] + " folder"
}

// imageRepo is an image without its registry, tag and digest: "linuxserver/jellyfin".
func imageRepo(img string) string {
	img = strings.ToLower(strings.TrimSpace(img))
	if i := strings.Index(img, "@"); i >= 0 {
		img = img[:i]
	}
	if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
		img = img[:i]
	}
	parts := strings.Split(img, "/")
	if len(parts) > 1 && strings.ContainsAny(parts[0], ".:") {
		parts = parts[1:]
	}
	if len(parts) > 1 && parts[0] == "library" {
		parts = parts[1:]
	}
	return strings.Join(parts, "/")
}

var casaAppDir = regexp.MustCompile(`^/DATA/AppData/[^/]+/?`)

// casaSource rewrites a volume's source. keep is false for one that only exists on other hardware.
func casaSource(src string) (out string, keep bool) {
	switch {
	case strings.HasPrefix(src, "/opt/vc"): // Raspberry Pi only
		return "", false
	case casaAppDir.MatchString(src):
		rest := strings.Trim(casaAppDir.ReplaceAllString(src, ""), "/")
		if rest == "" {
			rest = "data"
		}
		return "./" + rest, true
	case src == "/DATA/AppData":
		return "./data", true
	}
	return src, true
}

var errCasaSkip = errors.New("not for this computer")

// convertCasa turns one CasaOS compose file into a recipe: its config and its compose file.
func convertCasa(raw []byte) (*casaConfig, []byte, error) {
	var probe struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, nil, err
	}
	id := strings.Trim(idRe.ReplaceAllString(strings.ToLower(probe.Name), "-"), "-")
	if id == "" {
		return nil, nil, errors.New("no name")
	}
	text := strings.NewReplacer("${AppID}", probe.Name, "$AppID", probe.Name).Replace(string(raw))
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, nil, err
	}
	x, _ := doc["x-casaos"].(map[string]any)
	services, _ := doc["services"].(map[string]any)
	if x == nil || len(services) == 0 {
		return nil, nil, errors.New("not a CasaOS app")
	}
	if archs, ok := x["architectures"].([]any); ok && len(archs) > 0 {
		fits := false
		for _, a := range archs {
			fits = fits || a == runtime.GOARCH
		}
		if !fits {
			return nil, nil, errCasaSkip
		}
	}
	title := casaText(x["title"])
	if title == "" {
		title = probe.Name
	}
	low := strings.ToLower(title + " " + id)
	if strings.Contains(low, "nvidia") || strings.Contains(low, "playit") { // needs a graphics card set up for Docker; ZeroOS has playit built in
		return nil, nil, errCasaSkip
	}
	if s, _ := x["scheme"].(string); s == "https" {
		return nil, nil, errCasaSkip // its page only answers over https with its own certificate
	}
	cfg := &casaConfig{ID: id, Name: title, Available: true, ShortDesc: casaText(x["tagline"]), Description: casaText(x["description"]),
		Website: casaText(x["website"]), Source: casaText(x["repo"]), FormFields: []FormField{}, Folders: map[string]FolderNeed{}, Images: []string{}}
	cfg.Custom.Casa = true
	if tips, ok := x["tips"].(map[string]any); ok {
		if t := casaText(tips["before_install"]); t != "" {
			cfg.Description = strings.TrimSpace(cfg.Description + "\n\n## Good to know\n\n" + t)
		}
	}
	if c := casaCategories[strings.ToLower(casaText(x["category"]))]; c != "" {
		cfg.Categories = []string{c}
	} else {
		cfg.Categories = []string{"utilities"}
	}
	if s, _ := x["icon"].(string); strings.HasPrefix(s, "https://") {
		cfg.Icon = s
	}
	if s, _ := x["index"].(string); s != "" && s != "/" && strings.HasPrefix(s, "/") {
		cfg.URLSuffix = s
	}
	cfg.Version = strings.TrimSpace(fmt.Sprint(x["version"]))
	if x["version"] == nil || cfg.Version == "" {
		cfg.Version = "1"
	}

	hints := map[string]string{} // host folder -> what the store says about it
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		svc, ok := services[n].(map[string]any)
		if !ok {
			return nil, nil, errors.New("empty service")
		}
		if rt, _ := svc["runtime"].(string); rt == "nvidia" {
			return nil, nil, errCasaSkip
		}
		said := map[string]string{} // container folder -> description
		if sx, ok := svc["x-casaos"].(map[string]any); ok {
			if vs, ok := sx["volumes"].([]any); ok {
				for _, v := range vs {
					if vm, ok := v.(map[string]any); ok {
						said[fmt.Sprint(vm["container"])] = casaText(vm["description"])
					}
				}
			}
		}
		delete(svc, "x-casaos")
		if vols, ok := svc["volumes"].([]any); ok {
			kept := vols[:0]
			for _, v := range vols {
				src, target, ok := volumeSpec(v)
				if !ok || src == "" {
					kept = append(kept, v)
					continue
				}
				to, keep := casaSource(src)
				if !keep {
					continue
				}
				switch vv := v.(type) {
				case string:
					v = to + strings.TrimPrefix(vv, src)
				case map[string]any:
					vv["source"] = to
				}
				if strings.HasPrefix(to, "/") && said[target] != "" {
					hints[to] = said[target]
				}
				kept = append(kept, v)
			}
			svc["volumes"] = kept
		}
	}
	delete(doc, "x-casaos")
	delete(doc, "name")
	delete(doc, "version")

	// where the app's page is
	main, _ := x["main"].(string)
	if _, ok := services[main]; !ok {
		main = names[0]
	}
	portMap := strings.TrimSpace(fmt.Sprint(x["port_map"]))
	if x["port_map"] == nil {
		portMap = ""
	}
	if msvc, ok := services[main].(map[string]any); ok { // what the app itself runs, not its database
		if img, _ := msvc["image"].(string); img != "" {
			cfg.Images = []string{imageRepo(img)}
		}
	}
	page := CustomPage{Kind: "none"}
	if portMap != "" {
		find := func(n string) bool {
			svc, _ := services[n].(map[string]any)
			ports, _ := svc["ports"].([]any)
			for _, p := range ports {
				if pub, target, proto, ok := portSpec(p); ok && proto == "tcp" && pub == portMap {
					page = CustomPage{Kind: "port", Service: n, Port: target}
					return true
				}
			}
			return false
		}
		found := find(main)
		for _, n := range names {
			found = found || find(n)
		}
		if !found {
			var port int
			if _, err := fmt.Sscanf(portMap, "%d", &port); err != nil || port < 1 || port > 65535 {
				return nil, nil, errors.New("page port not found")
			}
			// the app listens on this port itself (it runs on the computer's own network)
			page = CustomPage{Kind: "fixed", Port: port}
		}
	}
	cfg.Custom.Page, cfg.NoGUI = page, page.Kind == "none"

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	chk, _, err := CheckCompose(string(out))
	if err != nil {
		return nil, nil, err
	}
	if err := checkPage(chk, page); err != nil {
		return nil, nil, err
	}
	for _, f := range chk.Folders {
		hint := hints[f.Key]
		if hint == "" {
			hint = "Pick a folder on your NAS. The app sees it as a normal folder."
		} else {
			hint = strings.TrimSuffix(hint, ".") + ". Pick a folder on your NAS, or leave it to keep these files on the mini PC."
		}
		cfg.Folders[f.Key] = FolderNeed{Key: f.Key, Label: casaFolderLabel(f.Key), Hint: hint}
	}
	for _, v := range chk.Vars {
		cfg.FormFields = append(cfg.FormFields, FormField{Type: "text", Label: v, EnvVariable: v, Hint: "Leave this empty if you don't use it."})
	}
	return cfg, out, nil
}

// syncCasa downloads the CasaOS store and converts it. Icons are fetched once and kept.
func (cat *Catalog) syncCasa() error {
	src := CasaOSURL
	if v := os.Getenv("ZEROOS_CASAOS_URL"); v != "" {
		src = v // for offline testing: a file:// path to the same zip, or "off"
	}
	if src == "off" {
		return nil
	}
	var raw []byte
	var err error
	if strings.HasPrefix(src, "file://") {
		raw, err = os.ReadFile(strings.TrimPrefix(src, "file://"))
	} else {
		raw, err = httpGet(src, 60<<20, 3*time.Minute)
	}
	if err != nil {
		return fmt.Errorf("couldn't download the CasaOS app catalog: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("the CasaOS app catalog can't be read: %w", err)
	}
	tmp := cat.casaDir() + ".new"
	_ = os.RemoveAll(tmp)
	icons := map[string]string{}
	n := 0
	for _, f := range zr.File {
		dir, file := path.Split(f.Name)
		if file != "docker-compose.yml" || path.Base(path.Dir(path.Clean(dir))) != "Apps" || f.UncompressedSize64 > 2<<20 {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(rc, 2<<20))
		rc.Close()
		if err != nil {
			continue
		}
		cfg, yml, err := convertCasa(b)
		if err != nil {
			continue // not for this computer, or a shape ZeroOS can't run yet
		}
		d := filepath.Join(tmp, cfg.ID)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		cj, _ := json.MarshalIndent(cfg, "", "  ")
		if os.WriteFile(filepath.Join(d, "config.json"), cj, 0o644) != nil || os.WriteFile(filepath.Join(d, "docker-compose.yml"), yml, 0o644) != nil {
			continue
		}
		if cfg.Icon != "" {
			icons[cfg.ID] = cfg.Icon
		}
		n++
	}
	if n == 0 {
		_ = os.RemoveAll(tmp)
		return errors.New("the CasaOS app catalog download was empty")
	}
	old := cat.casaDir() + ".old"
	_ = os.RemoveAll(old)
	_ = os.Rename(cat.casaDir(), old)
	if err := os.Rename(tmp, cat.casaDir()); err != nil {
		_ = os.Rename(old, cat.casaDir())
		return err
	}
	_ = os.RemoveAll(old)
	cat.fetchCasaIcons(icons)
	return nil
}

func httpGet(url string, max int64, wait time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: wait}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("too big")
	}
	return b, nil
}

// fetchCasaIcons downloads the icons that aren't there yet, a few at a time,
// and gives up after a minute; what is missing comes with the next refresh.
func (cat *Catalog) fetchCasaIcons(icons map[string]string) {
	dir := cat.casaIconDir()
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	type job struct{ id, url string }
	jobs := make(chan job)
	deadline := time.Now().Add(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if time.Now().After(deadline) {
					continue
				}
				b, err := httpGet(j.url, 2<<20, 15*time.Second)
				if err != nil || len(b) < 16 {
					continue
				}
				kind := http.DetectContentType(b)
				if !strings.HasPrefix(kind, "image/") && !bytes.Contains(b[:min(len(b), 512)], []byte("<svg")) {
					continue
				}
				ext := ".png"
				if bytes.Contains(b[:min(len(b), 512)], []byte("<svg")) {
					ext = ".svg"
				}
				tmp := filepath.Join(dir, j.id+".part")
				if os.WriteFile(tmp, b, 0o644) == nil {
					_ = os.Rename(tmp, filepath.Join(dir, j.id+ext))
				}
			}
		}()
	}
	for id, url := range icons {
		if cat.casaIcon(id) == "" {
			jobs <- job{id, url}
		}
	}
	close(jobs)
	wg.Wait()
}

func (cat *Catalog) casaIcon(id string) string {
	for _, ext := range []string{".png", ".svg"} {
		p := filepath.Join(cat.casaIconDir(), id+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// helperImage are databases and the like that many apps bring along; two apps
// sharing one of these are not the same app.
var helperImage = map[string]bool{"postgres": true, "postgresql": true, "redis": true, "valkey": true, "mariadb": true, "mysql": true, "mongo": true,
	"mongodb": true, "memcached": true, "nginx": true, "caddy": true, "pgvector": true, "busybox": true, "alpine": true, "elasticsearch": true,
	"minio": true, "rabbitmq": true, "clickhouse-server": true, "qdrant": true, "meilisearch": true, "docker": true, "postgis": true}

var plainName = regexp.MustCompile(`[^a-z0-9]+`)

// loadCasa adds the converted CasaOS apps that the catalogs loaded before it don't have.
func (cat *Catalog) loadCasa(apps map[string]*StoreApp) {
	entries, err := os.ReadDir(cat.casaDir())
	if err != nil {
		return
	}
	have := map[string]bool{}
	for id, a := range apps {
		have[plainName.ReplaceAllString(strings.ToLower(id), "")] = true
		have[plainName.ReplaceAllString(strings.ToLower(a.Name), "")] = true
		for _, img := range a.images {
			if !helperImage[path.Base(img)] {
				have["image:"+img] = true
			}
		}
	}
	for _, e := range entries {
		dir := filepath.Join(cat.casaDir(), e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			continue
		}
		var c casaConfig
		if json.Unmarshal(b, &c) != nil || c.ID == "" || c.Custom.Page.Kind == "" {
			continue
		}
		dup := have[plainName.ReplaceAllString(c.ID, "")] || have[plainName.ReplaceAllString(strings.ToLower(c.Name), "")]
		for _, img := range c.Images {
			dup = dup || have["image:"+img]
		}
		if dup {
			continue
		}
		plan, err := loadCompose(dir)
		if err != nil {
			continue
		}
		a := &StoreApp{ID: c.ID, Catalog: "casaos", Name: c.Name, ShortDesc: c.ShortDesc, Description: c.Description, Categories: c.Categories,
			Version: c.Version, Website: c.Website, Source: c.Source, NoGUI: c.NoGUI, URLSuffix: c.URLSuffix, Form: c.FormFields, Folders: []FolderNeed{},
			UsesDocker: plan.usesDockerSocket(), dir: dir, images: c.Images}
		if c.Custom.Page.Kind == "fixed" {
			a.fixedPort = c.Custom.Page.Port
		}
		keys := make([]string, 0, len(c.Folders))
		for k := range c.Folders {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			a.Folders = append(a.Folders, c.Folders[k])
		}
		if a.Form == nil {
			a.Form = []FormField{}
		}
		apps[a.ID] = a
	}
}
