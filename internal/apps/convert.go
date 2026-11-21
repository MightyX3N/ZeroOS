package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
				if str, ok := v.(string); ok {
					out = append(out, strings.SplitN(str, ":", 2)[0])
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
		if strings.HasPrefix(real, in.DataDir) {
			// files like ".../config.yml" are created by the app itself
			if filepath.Ext(real) == "" || strings.HasSuffix(hp, "/") {
				out = append(out, real)
			}
		}
	}
	return out
}

// Build returns the docker-compose.yml content.
func (p *composePlan) Build(in buildInput) ([]byte, error) {
	var doc map[string]any
	if p.yamlDoc != nil {
		doc = p.buildFromYAML(in)
	} else {
		doc = p.buildFromJSON(in)
	}
	return yaml.Marshal(doc)
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
	delete(doc, "networks")
	delete(doc, "version")
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
