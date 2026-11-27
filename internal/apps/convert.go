package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// composePlan is a catalog app's services, ready to turn into a docker-compose.yml.
// Two catalog formats are understood: Runtipi's dynamic JSON (most apps) and plain compose YAML.
type composePlan struct {
	services []rtService // dynamic JSON format
	yamlDoc  map[string]any
}

type rtService struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	IsMain          bool              `json:"isMain"`
	InternalPort    json.Number       `json:"internalPort"`
	Command         any               `json:"command"`
	Entrypoint      any               `json:"entrypoint"`
	Environment     json.RawMessage   `json:"environment"`
	Volumes         []rtVolume        `json:"volumes"`
	AddPorts        []rtPort          `json:"addPorts"`
	DependsOn       any               `json:"dependsOn"`
	HealthCheck     map[string]any    `json:"healthCheck"`
	User            string            `json:"user"`
	NetworkMode     string            `json:"networkMode"`
	Privileged      bool              `json:"privileged"`
	CapAdd          []string          `json:"capAdd"`
	CapDrop         []string          `json:"capDrop"`
	Devices         []string          `json:"devices"`
	Hostname        string            `json:"hostname"`
	SecurityOpt     []string          `json:"securityOpt"`
	StdinOpen       bool              `json:"stdinOpen"`
	Tty             bool              `json:"tty"`
	Logging         map[string]any    `json:"logging"`
	Sysctls         map[string]any    `json:"sysctls"`
	Ulimits         map[string]any    `json:"ulimits"`
	ShmSize         string            `json:"shmSize"`
	WorkingDir      string            `json:"workingDir"`
	StopSignal      string            `json:"stopSignal"`
	StopGracePeriod string            `json:"stopGracePeriod"`
	Pid             string            `json:"pid"`
	Deploy          map[string]any    `json:"deploy"`
	ExtraLabels     map[string]string `json:"extraLabels"`
	ExtraHosts      []string          `json:"extraHosts"`
}

type rtVolume struct {
	HostPath      string `json:"hostPath"`
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly"`
}

type rtPort struct {
	HostPort      json.Number `json:"hostPort"`
	ContainerPort json.Number `json:"containerPort"`
	UDP           *bool       `json:"udp"`
	TCP           *bool       `json:"tcp"`
	Interface     string      `json:"interface"`
}

func loadCompose(dir string) (*composePlan, error) {
	if b, err := os.ReadFile(filepath.Join(dir, "docker-compose.json")); err == nil {
		var doc struct {
			Services []rtService `json:"services"`
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil || len(doc.Services) == 0 {
			return nil, errors.New("bad compose json")
		}
		return &composePlan{services: doc.Services}, nil
	}
	if b, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml")); err == nil {
		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, err
		}
		if _, ok := doc["services"].(map[string]any); !ok {
			return nil, errors.New("no services")
		}
		return &composePlan{yamlDoc: doc}, nil
	}
	return nil, errors.New("no compose file")
}

func (p *composePlan) hostPaths() []string {
	var out []string
	if p.yamlDoc != nil {
		for _, s := range p.yamlDoc["services"].(map[string]any) {
			sm, _ := s.(map[string]any)
			vols, _ := sm["volumes"].([]any)
			for _, v := range vols {
				if src, _, ok := volumeSpec(v); ok && src != "" {
					out = append(out, src)
				}
			}
		}
		return out
	}
	for _, s := range p.services {
		for _, v := range s.Volumes {
			out = append(out, v.HostPath)
		}
	}
	return out
}

// mainOnHost tells whether the service with the app's page runs on the
// computer's own network, and the port it says its page is on (0: not said).
func (p *composePlan) mainOnHost() (host bool, port int) {
	if p.yamlDoc != nil {
		services, _ := p.yamlDoc["services"].(map[string]any)
		for _, s := range services {
			sm, _ := s.(map[string]any)
			x, _ := sm["x-runtipi"].(map[string]any)
			if main, _ := x["is_main"].(bool); main {
				nm, _ := sm["network_mode"].(string)
				fmt.Sscan(fmt.Sprint(x["internal_port"]), &port)
				return nm == "host", port
			}
		}
		return false, 0
	}
	for _, s := range p.services {
		if s.IsMain {
			fmt.Sscan(s.InternalPort.String(), &port)
			return s.NetworkMode == "host", port
		}
	}
	return false, 0
}

// imageRepos lists what the app runs, without registry and tag.
func (p *composePlan) imageRepos() []string {
	var out []string
	if p.yamlDoc != nil {
		for _, s := range p.yamlDoc["services"].(map[string]any) {
			if sm, ok := s.(map[string]any); ok {
				if img, _ := sm["image"].(string); img != "" {
					out = append(out, imageRepo(img))
				}
			}
		}
		return out
	}
	for _, s := range p.services {
		if s.Image != "" {
			out = append(out, imageRepo(s.Image))
		}
	}
	return out
}

func (p *composePlan) usesDockerSocket() bool {
	for _, hp := range p.hostPaths() {
		if strings.Contains(hp, "docker.sock") {
			return true
		}
	}
	return false
}

// buildInput is everything needed to write one installed app's compose file.
type buildInput struct {
	AppID     string
	DataDir   string            // the app's own folder on the SSD
	SharedDir string            // stands in for Runtipi's whole "root folder"
	Port      int               // host port for the main web page
	Folders   map[string]string // folder template -> real host path
	UsesNAS   bool              // ZeroOS starts it itself, after the NAS is connected
	GPU       bool              // pass the graphics chip in
	RenderGID int
	Custom    *customInfo // set for an app of your own: its compose file is kept as written where possible
	LAN       *LANLease   // the main part's address on the home network
}

var varRe = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)(:-[^}]*)?\}`)

// hostPath maps a catalog volume path to a real path on this computer.
func (in buildInput) hostPath(hp string) string {
	if real, ok := in.Folders[hp]; ok {
		return real
	}
	// A folder below a picked folder (e.g. media/data/movies when media/data was picked).
	for tmpl, real := range in.Folders {
		if strings.HasPrefix(hp, tmpl+"/") {
			return real + strings.TrimPrefix(hp, tmpl)
		}
	}
	out := varRe.ReplaceAllStringFunc(hp, func(m string) string {
		sub := varRe.FindStringSubmatch(m)
		switch sub[1] {
		case "APP_DATA_DIR":
			return in.DataDir
		case "ROOT_FOLDER_HOST":
			return in.SharedDir
		}
		if sub[2] != "" {
			return strings.TrimPrefix(sub[2], ":-")
		}
		return m
	})
	if strings.HasPrefix(out, "./") {
		out = filepath.Join(in.DataDir, strings.TrimPrefix(out, "./"))
	}
	return out
}

// insidePath fills in the app's own folder in a path inside a container.
func (in buildInput) insidePath(cp string) string {
	return strings.ReplaceAll(cp, "${APP_DATA_DIR}", in.DataDir)
}

// dirsToCreate lists the app's own SSD folders, so they exist with the right owner.
func (p *composePlan) dirsToCreate(in buildInput) []string {
	var out []string
	for _, hp := range p.hostPaths() {
		real := in.hostPath(hp)
		if in.Custom != nil {
			real = in.customPath(hp)
		}
		if strings.HasPrefix(real, in.DataDir) {
			// files like ".../config.yml" are created by the app itself
			if filepath.Ext(real) == "" || strings.HasSuffix(hp, "/") {
				out = append(out, real)
			}
		}
	}
	if in.Custom != nil {
		for _, name := range p.namedVolumes() {
			out = append(out, in.volumeDir(name))
		}
	}
	return out
}

// volumeDir is where a named volume of an app of your own lives: in the app's
// folder, so it is part of the copy an update keeps.
func (in buildInput) volumeDir(name string) string {
	return filepath.Join(in.DataDir, "volumes", name)
}

// namedVolumes lists the compose file's own named volumes (not ones made elsewhere).
func (p *composePlan) namedVolumes() []string {
	var out []string
	vols, _ := p.yamlDoc["volumes"].(map[string]any)
	for name, v := range vols {
		if vm, ok := v.(map[string]any); ok {
			if ext, has := vm["external"]; has && ext != false {
				continue
			}
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Build returns the docker-compose.yml content.
func (p *composePlan) Build(in buildInput) ([]byte, error) {
	main := p.lanService(in)
	var doc map[string]any
	if p.yamlDoc != nil {
		doc = p.buildFromYAML(in)
	} else {
		doc = p.buildFromJSON(in)
	}
	if in.LAN != nil && main != "" {
		joinLAN(doc, main, in.LAN)
	}
	return yaml.Marshal(doc)
}

// lanService is the part of the app that joins the home network: its main
// part, unless that already runs on the computer's own network.
func (p *composePlan) lanService(in buildInput) string {
	if p.yamlDoc != nil {
		services, _ := p.yamlDoc["services"].(map[string]any)
		ok := func(n string) string {
			sm, _ := services[n].(map[string]any)
			if sm == nil {
				return ""
			}
			if nm, _ := sm["network_mode"].(string); nm != "" {
				return ""
			}
			return n
		}
		for n, s := range services {
			sm, _ := s.(map[string]any)
			x, _ := sm["x-runtipi"].(map[string]any)
			if main, _ := x["is_main"].(bool); main {
				return ok(n)
			}
		}
		if in.Custom != nil && in.Custom.Page.Kind == "port" && in.Custom.Page.Service != "" {
			return ok(in.Custom.Page.Service)
		}
		if len(services) == 1 {
			for n := range services {
				return ok(n)
			}
		}
		return ""
	}
	for _, s := range p.services {
		if s.IsMain {
			if s.NetworkMode != "" {
				return ""
			}
			return s.Name
		}
	}
	if len(p.services) == 1 && p.services[0].NetworkMode == "" {
		return p.services[0].Name
	}
	return ""
}

// joinLAN puts a service on the home network too, with its own address. It
// stays on the app's private network, where its other parts find it.
func joinLAN(doc map[string]any, name string, l *LANLease) {
	services, _ := doc["services"].(map[string]any)
	svc, _ := services[name].(map[string]any)
	if svc == nil {
		return
	}
	nets := map[string]any{}
	switch v := svc["networks"].(type) {
	case []any:
		for _, n := range v {
			nets[fmt.Sprint(n)] = nil
		}
	case map[string]any:
		for k, n := range v {
			nets[k] = n
		}
	}
	if len(nets) == 0 {
		nets["default"] = nil
	}
	nets[lanNet] = map[string]any{"ipv4_address": l.IP, "mac_address": l.MAC}
	svc["networks"] = nets
	top, _ := doc["networks"].(map[string]any)
	if top == nil {
		top = map[string]any{}
	}
	top[lanNet] = map[string]any{"external": true, "name": lanNet}
	doc["networks"] = top
}

func (p *composePlan) restartPolicy(in buildInput) string {
	if in.UsesNAS {
		return "no" // ZeroOS starts it once the NAS is there, and restarts it if it stops
	}
	return "unless-stopped"
}

func (p *composePlan) buildFromJSON(in buildInput) map[string]any {
	services := map[string]any{}
	for _, s := range p.services {
		svc := map[string]any{
			"image":   s.Image,
			"restart": p.restartPolicy(in),
			"labels":  map[string]any{"zeroos.app": in.AppID},
		}
		if s.Command != nil {
			svc["command"] = s.Command
		}
		if s.Entrypoint != nil {
			svc["entrypoint"] = s.Entrypoint
		}
		if env := parseEnv(s.Environment); len(env) > 0 {
			svc["environment"] = env
		}
		var vols []string
		for _, v := range s.Volumes {
			// The path inside may name the app's folder too: a few apps (ones that
			// start containers themselves) need it at the same place inside and out.
			spec := in.hostPath(v.HostPath) + ":" + in.insidePath(v.ContainerPath)
			if v.ReadOnly {
				spec += ":ro"
			}
			vols = append(vols, spec)
		}
		if len(vols) > 0 {
			svc["volumes"] = vols
		}
		var ports []string
		if s.NetworkMode == "" {
			if s.IsMain && s.InternalPort.String() != "" && in.Port > 0 {
				ports = append(ports, fmt.Sprintf("%d:%s", in.Port, s.InternalPort.String()))
			}
			for _, ap := range s.AddPorts {
				prefix := ""
				if ap.Interface != "" {
					prefix = ap.Interface + ":"
				}
				tcp := ap.TCP == nil || *ap.TCP
				udp := ap.UDP != nil && *ap.UDP
				if tcp {
					ports = append(ports, fmt.Sprintf("%s%s:%s", prefix, ap.HostPort.String(), ap.ContainerPort.String()))
				}
				if udp {
					ports = append(ports, fmt.Sprintf("%s%s:%s/udp", prefix, ap.HostPort.String(), ap.ContainerPort.String()))
				}
			}
		} else {
			svc["network_mode"] = s.NetworkMode
		}
		if len(ports) > 0 {
			svc["ports"] = ports
		}
		if s.DependsOn != nil {
			svc["depends_on"] = s.DependsOn
		}
		if len(s.HealthCheck) > 0 {
			hc := map[string]any{}
			for k, v := range s.HealthCheck {
				switch k {
				case "test":
					if str, ok := v.(string); ok {
						hc["test"] = []string{"CMD-SHELL", str}
					} else {
						hc["test"] = v
					}
				case "startPeriod":
					hc["start_period"] = v
				case "startInterval":
					hc["start_interval"] = v
				default:
					hc[k] = v
				}
			}
			svc["healthcheck"] = hc
		}
		set := func(k string, v any, ok bool) {
			if ok {
				svc[k] = v
			}
		}
		set("user", s.User, s.User != "")
		set("privileged", true, s.Privileged)
		set("cap_add", s.CapAdd, len(s.CapAdd) > 0)
		set("cap_drop", s.CapDrop, len(s.CapDrop) > 0)
		set("hostname", s.Hostname, s.Hostname != "")
		set("security_opt", s.SecurityOpt, len(s.SecurityOpt) > 0)
		set("stdin_open", true, s.StdinOpen)
		set("tty", true, s.Tty)
		set("logging", s.Logging, len(s.Logging) > 0)
		set("sysctls", s.Sysctls, len(s.Sysctls) > 0)
		set("ulimits", s.Ulimits, len(s.Ulimits) > 0)
		set("shm_size", s.ShmSize, s.ShmSize != "")
		set("working_dir", s.WorkingDir, s.WorkingDir != "")
		set("stop_signal", s.StopSignal, s.StopSignal != "")
		set("stop_grace_period", s.StopGracePeriod, s.StopGracePeriod != "")
		set("pid", s.Pid, s.Pid != "")
		set("deploy", s.Deploy, len(s.Deploy) > 0)
		set("extra_hosts", s.ExtraHosts, len(s.ExtraHosts) > 0)
		devices := append([]string{}, s.Devices...)
		if in.GPU && s.IsMain {
			devices = append(devices, "/dev/dri:/dev/dri")
			if in.RenderGID > 0 {
				svc["group_add"] = []string{fmt.Sprint(in.RenderGID)}
			}
		}
		set("devices", devices, len(devices) > 0)
		services[s.Name] = svc
	}
	return map[string]any{"services": services}
}

func parseEnv(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var list []struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	}
	if json.Unmarshal(raw, &list) == nil {
		for _, kv := range list {
			out[kv.Key] = fmt.Sprint(kv.Value)
		}
		return out
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// buildFromYAML adapts a plain compose file: drops Runtipi's own network and
// proxy labels, and maps folders the same way as the JSON format.
func (p *composePlan) buildFromYAML(in buildInput) map[string]any {
	// deep copy through YAML
	b, _ := yaml.Marshal(p.yamlDoc)
	var doc map[string]any
	_ = yaml.Unmarshal(b, &doc)
	if in.Custom != nil {
		return p.buildCustom(in, doc)
	}
	delete(doc, "networks")
	delete(doc, "version")
	delete(doc, "x-runtipi")
	services := doc["services"].(map[string]any)
	for _, s := range services {
		svc, ok := s.(map[string]any)
		if !ok {
			continue
		}
		delete(svc, "networks")
		delete(svc, "container_name")
		svc["labels"] = map[string]any{"zeroos.app": in.AppID}
		svc["restart"] = p.restartPolicy(in)
		// Runtipi's newer catalog format says where the app's page is in an
		// "x-runtipi" part: that port goes on the port ZeroOS picked.
		if x, ok := svc["x-runtipi"].(map[string]any); ok {
			main, _ := x["is_main"].(bool)
			if port := fmt.Sprint(x["internal_port"]); main && x["internal_port"] != nil && port != "" && in.Port > 0 {
				if nm, _ := svc["network_mode"].(string); nm != "host" {
					ports, _ := svc["ports"].([]any)
					svc["ports"] = append([]any{fmt.Sprintf("%d:%s", in.Port, port)}, ports...)
				}
			}
			if main && in.GPU {
				devs, _ := svc["devices"].([]any)
				svc["devices"] = append(devs, "/dev/dri:/dev/dri")
				if in.RenderGID > 0 {
					groups, _ := svc["group_add"].([]any)
					svc["group_add"] = append(groups, fmt.Sprint(in.RenderGID))
				}
			}
		}
		delete(svc, "x-runtipi")
		if vols, ok := svc["volumes"].([]any); ok {
			for i, v := range vols {
				if str, ok := v.(string); ok {
					parts := strings.SplitN(str, ":", 2)
					if len(parts) == 2 {
						vols[i] = in.hostPath(parts[0]) + ":" + parts[1]
					}
				}
			}
		}
	}
	return doc
}

// buildCustom adapts an app of your own. It runs as written, except:
// the page's port goes on the port ZeroOS picked, named volumes and relative
// folders live in the app's folder, picked folders replace the ones written,
// and ZeroOS adds its label and decides when the app restarts.
func (p *composePlan) buildCustom(in buildInput, doc map[string]any) map[string]any {
	delete(doc, "version")
	services, _ := doc["services"].(map[string]any)
	page := in.Custom.Page
	for name, s := range services {
		svc, ok := s.(map[string]any)
		if !ok {
			continue
		}
		delete(svc, "build")
		svc["restart"] = p.restartPolicy(in)
		// A store app written for several kinds of computer names devices this one may lack.
		if devs, ok := svc["devices"].([]any); ok && in.Custom.Casa {
			var kept []any
			for _, d := range devs {
				if s, ok := d.(string); ok {
					if _, err := os.Stat(strings.SplitN(s, ":", 2)[0]); err != nil {
						continue
					}
				}
				kept = append(kept, d)
			}
			if len(kept) == 0 {
				delete(svc, "devices")
			} else {
				svc["devices"] = kept
			}
		}
		// labels: keep the file's own, add ours
		switch l := svc["labels"].(type) {
		case map[string]any:
			l["zeroos.app"] = in.AppID
		case []any:
			svc["labels"] = append(l, "zeroos.app="+in.AppID)
		default:
			svc["labels"] = map[string]any{"zeroos.app": in.AppID}
		}
		if ports, ok := svc["ports"].([]any); ok && page.Kind == "port" && page.Service == name && in.Port > 0 {
			moved := false
			var kept []any
			for _, pv := range ports {
				_, target, proto, ok := portSpec(pv)
				if ok && !moved && target == page.Port && proto == "tcp" {
					kept = append(kept, fmt.Sprintf("%d:%d", in.Port, target))
					moved = true
					continue
				}
				kept = append(kept, pv)
			}
			svc["ports"] = kept
		}
		if vols, ok := svc["volumes"].([]any); ok {
			for i, v := range vols {
				switch x := v.(type) {
				case string:
					parts := strings.SplitN(x, ":", 2)
					if len(parts) == 2 {
						vols[i] = in.customPath(parts[0]) + ":" + parts[1]
					}
				case map[string]any:
					if src, _ := x["source"].(string); src != "" {
						if t, _ := x["type"].(string); t == "bind" || strings.ContainsAny(src, "/.~") {
							x["source"] = in.customPath(src)
						}
					}
				}
			}
		}
	}
	// named volumes keep working as volumes, but their files live in the app's folder
	if vols, ok := doc["volumes"].(map[string]any); ok {
		for _, name := range p.namedVolumes() {
			vols[name] = map[string]any{"driver": "local", "driver_opts": map[string]any{"type": "none", "o": "bind", "device": in.volumeDir(name)}}
		}
	}
	return doc
}

// customPath maps a folder written in a compose file to a real place: a picked
// folder, the app's own folder for relative paths, or itself (named volumes
// and paths of the computer itself).
func (in buildInput) customPath(src string) string {
	if real, ok := in.Folders[src]; ok {
		return real
	}
	switch {
	case strings.HasPrefix(src, "./"), src == ".":
		return filepath.Join(in.DataDir, "files", strings.TrimPrefix(src, "."))
	case strings.HasPrefix(src, "../"):
		return filepath.Join(in.DataDir, "files", strings.ReplaceAll(src, "../", ""))
	}
	return src
}
