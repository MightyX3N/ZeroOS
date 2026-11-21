package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
	"gopkg.in/yaml.v3"
)

// Your own apps, from a docker-compose.yml.
//
// A pasted compose file becomes an app like any other: an icon, Open, Share
// online, updates with undo. ZeroOS changes as little as it can:
//   - the port of the app's page goes on a free port ZeroOS picks
//   - named volumes and relative folders land in the app's own folder on the SSD
//   - other folders are asked for, with the folder picker
// Everything else runs as written.

// CustomPage says where the app's page is.
type CustomPage struct {
	Kind    string `json:"kind"`    // "port": a published port; "fixed": the app listens on this port itself; "none": no page
	Service string `json:"service"` // for "port"
	Port    int    `json:"port"`    // container port for "port", the port itself for "fixed"
}

// customInfo is what a custom app's recipe adds to its compose file.
type customInfo struct {
	Page CustomPage `json:"page"`
}

// customConfig is the config.json of a custom app's recipe.
type customConfig struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Available bool       `json:"available"`
	Version   string     `json:"version"`
	NoGUI     bool       `json:"no_gui"`
	Custom    customInfo `json:"custom"`
}

// CustomPort is a port a compose file publishes.
type CustomPort struct {
	Service   string `json:"service"`
	Container int    `json:"container"`
	Published string `json:"published"`
	Proto     string `json:"proto"`
}

// CustomFolder is a folder on this computer that a compose file names.
type CustomFolder struct {
	Key       string `json:"key"` // as written in the file
	Service   string `json:"service"`
	Container string `json:"container"` // where the app sees it
}

// CustomCheck is what ZeroOS found in a compose file.
type CustomCheck struct {
	Services []string       `json:"services"` // "name (image)"
	Ports    []CustomPort   `json:"ports"`
	Folders  []CustomFolder `json:"folders"`  // asked for with the folder picker
	Vars     []string       `json:"vars"`     // ${NAMES} the file expects to be filled in
	Warnings []string       `json:"warnings"` // things it may do to the whole computer
	HostNet  bool           `json:"hostNet"`  // a part runs on the computer's own network
}

// systemPath tells a path of the computer itself (left exactly as written)
// from a folder for files (asked for).
func systemPath(p string) bool {
	for _, pre := range []string{"/var/run/", "/run/", "/dev/", "/proc/", "/sys/", "/etc/", "/lib/", "/usr/", "/boot/", "/tmp/", "/var/lib/docker/"} {
		if strings.HasPrefix(p+"/", pre) {
			return true
		}
	}
	return p == "/"
}

var composeVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?[-?+][^}]*)?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// givenVars are filled in by ZeroOS for every app.
var givenVars = map[string]bool{"TZ": true, "PUID": true, "PGID": true, "UID": true, "GID": true, "APP_PORT": true, "APP_DATA_DIR": true,
	"APP_ID": true, "HOST_NAME": true, "APP_HOST": true, "APP_DOMAIN": true, "INTERNAL_IP": true, "ROOT_FOLDER_HOST": true}

// portSpec reads one entry of a service's "ports".
func portSpec(v any) (published string, target int, proto string, ok bool) {
	proto = "tcp"
	switch x := v.(type) {
	case int:
		return "", x, proto, true
	case string:
		s := x
		if i := strings.Index(s, "/"); i >= 0 {
			proto, s = s[i+1:], s[:i]
		}
		parts := strings.Split(s, ":")
		t, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			return "", 0, proto, false // a range, or something else we leave alone
		}
		if len(parts) >= 2 {
			published = parts[len(parts)-2]
		}
		return published, t, proto, true
	case map[string]any:
		switch t := x["target"].(type) {
		case int:
			target = t
		case string:
			target, _ = strconv.Atoi(t)
		}
		if p, ok := x["protocol"].(string); ok && p != "" {
			proto = p
		}
		if p, ok := x["published"]; ok {
			published = fmt.Sprint(p)
		}
		return published, target, proto, target > 0
	}
	return "", 0, proto, false
}

// volumeSpec reads one entry of a service's "volumes": source, target and whether it is a bind.
func volumeSpec(v any) (source, target string, ok bool) {
	switch x := v.(type) {
	case string:
		parts := strings.SplitN(x, ":", 3)
		if len(parts) < 2 {
			return "", parts[0], true // an anonymous volume
		}
		return parts[0], parts[1], true
	case map[string]any:
		s, _ := x["source"].(string)
		t, _ := x["target"].(string)
		return s, t, t != ""
	}
	return "", "", false
}

// CheckCompose reads a compose file and says what ZeroOS will ask and warn about.
// It refuses files ZeroOS can't run as they are, in plain words.
func CheckCompose(text string) (*CustomCheck, map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil, errors.New("Paste a compose file first.")
	}
	if len(text) > 200_000 {
		return nil, nil, errors.New("That compose file is too big.")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		msg := strings.TrimPrefix(err.Error(), "yaml: ")
		return nil, nil, fmt.Errorf("ZeroOS can't read this as a compose file (%s). Check that the spaces at the start of each line are as in the original.", msg)
	}
	services, _ := doc["services"].(map[string]any)
	if len(services) == 0 {
		return nil, nil, errors.New("This compose file has no \"services:\" part, so there is nothing to run.")
	}
	for _, k := range []string{"include", "extends"} {
		if _, ok := doc[k]; ok {
			return nil, nil, fmt.Errorf("This compose file pulls in other files (\"%s:\"), which ZeroOS doesn't have. Paste one complete file.", k)
		}
	}
	out := &CustomCheck{Services: []string{}, Ports: []CustomPort{}, Folders: []CustomFolder{}, Vars: []string{}, Warnings: []string{}}
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	warn := map[string]bool{}
	seenFolder := map[string]bool{}
	for _, n := range names {
		svc, ok := services[n].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("The part \"%s\" in this compose file is empty.", n)
		}
		img, _ := svc["image"].(string)
		if _, ok := svc["build"]; ok && img == "" {
			return nil, nil, fmt.Errorf("The part \"%s\" has to be built from source code (\"build:\"), which isn't in a compose file. ZeroOS runs ready-made images: use a file that has \"image:\" for every part.", n)
		}
		if img == "" {
			return nil, nil, fmt.Errorf("The part \"%s\" has no \"image:\", so ZeroOS doesn't know what to run.", n)
		}
		delete(svc, "build")
		if _, ok := svc["env_file"]; ok {
			return nil, nil, fmt.Errorf("The part \"%s\" reads its settings from a separate file (\"env_file:\"), which ZeroOS doesn't have. Put those lines under \"environment:\" instead.", n)
		}
		if _, ok := svc["extends"]; ok {
			return nil, nil, fmt.Errorf("The part \"%s\" builds on another file (\"extends:\"), which ZeroOS doesn't have. Paste one complete file.", n)
		}
		out.Services = append(out.Services, fmt.Sprintf("%s (%s)", n, img))
		if nm, _ := svc["network_mode"].(string); nm == "host" {
			out.HostNet = true
			warn["A part of it runs directly on this mini PC's network, so it can use any port."] = true
		}
		if b, _ := svc["privileged"].(bool); b {
			warn["A part of it runs with full control over this mini PC (\"privileged\")."] = true
		}
		if pid, _ := svc["pid"].(string); pid == "host" {
			warn["A part of it can see every program running on this mini PC."] = true
		}
		if ports, ok := svc["ports"].([]any); ok {
			for _, p := range ports {
				pub, target, proto, ok := portSpec(p)
				if !ok {
					continue
				}
				out.Ports = append(out.Ports, CustomPort{Service: n, Container: target, Published: pub, Proto: proto})
			}
		}
		if vols, ok := svc["volumes"].([]any); ok {
			for _, v := range vols {
				src, target, ok := volumeSpec(v)
				if !ok || src == "" {
					continue
				}
				switch {
				case strings.Contains(src, "docker.sock"):
					warn["It controls Docker on the whole mini PC (it uses the Docker socket)."] = true
				case strings.HasPrefix(src, "/") && systemPath(src):
					warn[fmt.Sprintf("It uses %s of this mini PC itself.", src)] = true
				case strings.HasPrefix(src, "/") || strings.HasPrefix(src, "~"):
					if !seenFolder[src] {
						seenFolder[src] = true
						out.Folders = append(out.Folders, CustomFolder{Key: src, Service: n, Container: target})
					}
				}
			}
		}
	}
	for w := range warn {
		out.Warnings = append(out.Warnings, w)
	}
	sort.Strings(out.Warnings)
	seen := map[string]bool{}
	for _, m := range composeVar.FindAllStringSubmatch(text, -1) {
		name, hasDefault := m[1], strings.Contains(m[2], "-")
		if name == "" {
			name = m[3]
		}
		if name == "" || hasDefault || givenVars[name] || seen[name] {
			continue
		}
		seen[name] = true
		out.Vars = append(out.Vars, name)
	}
	return out, doc, nil
}

// checkPage makes sure the chosen page exists in the file.
func checkPage(c *CustomCheck, page CustomPage) error {
	switch page.Kind {
	case "none":
		return nil
	case "fixed":
		if page.Port < 1 || page.Port > 65535 {
			return errors.New("Type the port of the app's page, between 1 and 65535.")
		}
		return nil
	case "port":
		for _, p := range c.Ports {
			if p.Service == page.Service && p.Container == page.Port && p.Proto == "tcp" {
				return nil
			}
		}
		return errors.New("That port isn't in the compose file any more. Choose the app's page again.")
	}
	return errors.New("Choose where the app's page is.")
}

// CustomRequest is everything needed to add or change a custom app.
type CustomRequest struct {
	Name    string            `json:"name"`
	Compose string            `json:"compose"`
	Page    CustomPage        `json:"page"`
	Folders map[string]Picked `json:"folders"`
	Env     map[string]string `json:"env"`
}

// writeCustomRecipe writes a custom app's recipe (its compose file and what ZeroOS adds to it).
func writeCustomRecipe(dir, id string, req CustomRequest) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cfg, _ := json.MarshalIndent(customConfig{ID: id, Name: req.Name, Available: true, Version: "custom",
		NoGUI: req.Page.Kind == "none", Custom: customInfo{Page: req.Page}}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0o644); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir, "docker-compose.json"))
	return os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(req.Compose), 0o644)
}

// readCustom reads a recipe's custom part, if it is a custom app's recipe.
func readCustom(dir string) (*customConfig, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, false
	}
	var c customConfig
	if json.Unmarshal(b, &c) != nil || c.Custom.Page.Kind == "" {
		return nil, false
	}
	return &c, true
}

// prepareCustom validates a request against its compose file and resolves folders.
func (m *Manager) prepareCustom(req *CustomRequest, appID string) (*CustomCheck, map[string]Picked, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 40 {
		return nil, nil, errors.New("Give the app a name of up to 40 characters.")
	}
	chk, _, err := CheckCompose(req.Compose)
	if err != nil {
		return nil, nil, err
	}
	if err := checkPage(chk, req.Page); err != nil {
		return nil, nil, err
	}
	for _, v := range chk.Vars {
		if strings.TrimSpace(req.Env[v]) == "" {
			return nil, nil, fmt.Errorf("Fill in \"%s\". The compose file expects it.", v)
		}
	}
	folders := map[string]Picked{}
	for _, f := range chk.Folders {
		p, ok := req.Folders[f.Key]
		if !ok || p.Root == "" {
			p = Picked{Root: "local", Path: "/" + appID + "/" + filepath.Base(f.Key)}
		}
		if _, err := m.st.Resolve(p.Root, p.Path); err != nil {
			return nil, nil, err
		}
		if !m.st.RootConnected(p.Root) {
			return nil, nil, errors.New("That NAS share isn't connected right now.")
		}
		folders[f.Key] = p
	}
	return chk, folders, nil
}

func (m *Manager) portTaken(port int, exceptID string) bool {
	var id string
	return m.c.DB.QueryRow(`SELECT id FROM apps WHERE port=? AND id<>?`, port, exceptID).Scan(&id) == nil
}

// InstallCustom adds an app from a compose file.
func (m *Manager) InstallCustom(req CustomRequest, userID int64) (*Job, error) {
	id := idRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(req.Name)), "-")
	id = strings.Trim(id, "-")
	if id == "" {
		id = "app"
	}
	if _, err := m.Get(id); err == nil {
		for i := 2; ; i++ {
			if _, err := m.Get(fmt.Sprintf("%s-%d", id, i)); err != nil {
				id = fmt.Sprintf("%s-%d", id, i)
				break
			}
		}
	}
	_, folders, err := m.prepareCustom(&req, id)
	if err != nil {
		return nil, err
	}
	port := 0
	switch req.Page.Kind {
	case "fixed":
		port = req.Page.Port
		if m.portTaken(port, id) {
			return nil, fmt.Errorf("Port %d is already the page of another app.", port)
		}
	default:
		if port, err = m.freePort(); err != nil {
			return nil, err
		}
	}
	// the recipe waits next to the app's folder until the install picks it up
	if err := writeCustomRecipe(filepath.Join(m.appDir(id), "recipe.new"), id, req); err != nil {
		return nil, err
	}
	settings, _ := json.Marshal(AppSettings{Env: cleanEnv(req.Env), Desired: "running", Version: "custom"})
	fj, _ := json.Marshal(folders)
	if _, err := m.c.DB.Exec(`INSERT INTO apps(id,store_id,catalog,name,port,state,settings,folders,installed_by,created) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, "custom", "custom", req.Name, port, "installing", string(settings), string(fj), userID, time.Now().Unix()); err != nil {
		return nil, err
	}
	job := &Job{ID: core.RandomHex(8), AppID: id}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()
	go m.runInstall(job, id)
	return job, nil
}

func cleanEnv(env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		if composeVar.MatchString("${"+k+"}") && strings.TrimSpace(v) != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// CustomSource returns what a custom app was made from, for editing it.
func (m *Manager) CustomSource(id string) (*CustomRequest, error) {
	a, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	cfg, ok := readCustom(m.recipeDir(id))
	if !ok {
		return nil, errors.New("This app came from the Store, so it has no compose file of your own.")
	}
	b, err := os.ReadFile(filepath.Join(m.recipeDir(id), "docker-compose.yml"))
	if err != nil {
		return nil, err
	}
	return &CustomRequest{Name: a.Name, Compose: string(b), Page: cfg.Custom.Page, Folders: a.Folders, Env: a.Settings.Env}, nil
}

// EditCustom changes a custom app's compose file. It goes through the same
// steps as an update: a copy first, put back if the new file doesn't start.
func (m *Manager) EditCustom(id string, req CustomRequest, noUndo bool) (*Job, error) {
	a, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if _, ok := readCustom(m.recipeDir(id)); !ok {
		return nil, errors.New("This app came from the Store, so its compose file can't be changed here.")
	}
	_, folders, err := m.prepareCustom(&req, id)
	if err != nil {
		return nil, err
	}
	// folders already chosen keep their place unless picked again
	for k, p := range a.Folders {
		if _, again := req.Folders[k]; !again {
			if _, still := folders[k]; still {
				folders[k] = p
			}
		}
	}
	port := a.Port
	if req.Page.Kind == "fixed" {
		port = req.Page.Port
		if m.portTaken(port, id) {
			return nil, fmt.Errorf("Port %d is already the page of another app.", port)
		}
	} else if cur, _ := readCustom(m.recipeDir(id)); cur != nil && cur.Custom.Page.Kind == "fixed" {
		if port, err = m.freePort(); err != nil {
			return nil, err
		}
	}
	if !noUndo {
		need := dirSize(filepath.Join(m.appDir(id), "data"))
		need += need/10 + 200<<20
		if free := freeSpace(m.appDir(id)); free < need {
			return nil, &ErrNoSpace{Need: need, Free: free}
		}
	}
	staged := filepath.Join(m.appDir(id), "recipe.new")
	_ = os.RemoveAll(staged)
	if err := writeCustomRecipe(staged, id, req); err != nil {
		return nil, err
	}
	if !m.lock(id) {
		_ = os.RemoveAll(staged)
		return nil, ErrBusy
	}
	job := m.newJob(id)
	go func() {
		defer m.unlock(id)
		defer os.RemoveAll(staged)
		m.runUpdate(job, id, noUndo, &recipeChange{dir: staged, name: req.Name, port: port, folders: folders, env: cleanEnv(req.Env)})
	}()
	return job, nil
}

// recipeChange is an edit of a custom app, applied by runUpdate in place of a newer catalog entry.
type recipeChange struct {
	dir     string
	name    string
	port    int
	folders map[string]Picked
	env     map[string]string
}
