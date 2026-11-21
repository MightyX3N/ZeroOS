// Package playittest is a stand-in for playit.gg's API, for tests. It accepts
// only requests shaped like the ones playit.gg's own website sends, and
// answers "failed to parse body" to anything else, like the real one does.
package playittest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const (
	Secret  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	AgentID = "5b1f6c0e-3f0a-4c58-9a51-0c6f2f6f1a11"
)

type Tunnel struct {
	ID, Name, Proto string
	LocalPort       string
	PublicPort      int
}

// HTTPS is a "Website (HTTPS)" tunnel as a person makes it on playit.gg.
type HTTPS struct {
	Name     string `json:"name"`
	Host     string `json:"host"`      // films.playit.plus
	HTTPPort int    `json:"http_port"` // local port
	Cert     bool   `json:"cert"`      // TLS Termination = letsencrypt
}

type Fake struct {
	mu      sync.Mutex
	Premium bool
	HTTPS   []HTTPS
	Tunnels []Tunnel
	Claims  map[string]int // claim code -> times polled
	n       int
}

func New() *Fake { return &Fake{Claims: map[string]int{}} }

func send(w http.ResponseWriter, status string, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "data": data})
}

func badBody(w http.ResponseWriter) {
	w.WriteHeader(400)
	send(w, "error", map[string]string{"type": "validation", "message": "failed to parse body"})
}

// strict decodes JSON and refuses fields the real API doesn't have.
func strict(r *http.Request, v any) bool {
	b, _ := io.ReadAll(r.Body)
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v) == nil
}

func ascii(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

type createReq struct {
	Name     *string `json:"name"`
	Protocol *struct {
		Type    string          `json:"type"`
		Details json.RawMessage `json:"details"`
	} `json:"protocol"`
	Origin *struct {
		Type string `json:"type"`
		Data *struct {
			AgentID string `json:"agent_id"`
			Config  *struct {
				Fields []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"fields"`
			} `json:"config"`
		} `json:"data"`
	} `json:"origin"`
	Endpoint *struct {
		Type    string `json:"type"`
		Details *struct {
			Region string `json:"region"`
			Port   *int   `json:"port"`
		} `json:"details"`
	} `json:"endpoint"`
	Enabled *bool `json:"enabled"`
}

type rawPorts struct {
	PortType string `json:"port_type"`
	Count    *int   `json:"port_count"`
	Desc     string `json:"software_description"`
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	if path == "/_tunnels" { // for tests: what the fake account holds
		_ = json.NewEncoder(w).Encode(f.Tunnels)
		return
	}
	if path == "/_account" { // for tests: what a person did on playit.gg
		var in struct {
			Premium bool    `json:"premium"`
			HTTPS   []HTTPS `json:"https"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			w.WriteHeader(400)
			return
		}
		f.Premium, f.HTTPS = in.Premium, in.HTTPS
		return
	}
	if strings.HasPrefix(path, "/claim/") {
		f.claim(w, r)
		return
	}
	switch r.Header.Get("Authorization") {
	case "":
		send(w, "error", map[string]string{"type": "auth", "message": "AuthRequired"})
		return
	case "Agent-Key " + Secret:
	default:
		send(w, "error", map[string]string{"type": "auth", "message": "InvalidAgentKey"})
		return
	}
	switch path {
	case "/v1/agents/rundata":
		var in struct{}
		if !strict(r, &in) {
			badBody(w)
			return
		}
		// Shaped like the real answer: what this agent runs, HTTPS tunnels included.
		list := []any{}
		field := func(n, v string) map[string]string { return map[string]string{"name": n, "value": v} }
		for i, t := range f.Tunnels {
			list = append(list, map[string]any{"id": t.ID, "internal_id": 5000 + i, "name": t.Name,
				"display_address": fmt.Sprintf("test-name.gl.at.ply.gg:%d", t.PublicPort), "port_type": t.Proto, "port_count": 1,
				"tunnel_type": nil, "tunnel_type_display": "TCP", "disabled_reason": nil,
				"agent_config": map[string]any{"fields": []any{field("local_ip", "127.0.0.1"), field("local_port", t.LocalPort)}}})
		}
		for i, h := range f.HTTPS {
			fields := []any{field("http_port", fmt.Sprint(h.HTTPPort)), field("https_port", "0"), field("local_ip", "127.0.0.1")}
			if h.Cert {
				fields = append(fields, field("tls_termination", "letsencrypt"))
			}
			list = append(list, map[string]any{"id": fmt.Sprintf("00000000-0000-4000-9000-%012d", i+1), "internal_id": 9000 + i, "name": h.Name,
				"display_address": h.Host, "port_type": "tcp", "port_count": 2,
				"tunnel_type": "https", "tunnel_type_display": "HTTPs", "disabled_reason": nil,
				"agent_config": map[string]any{"fields": fields}})
		}
		send(w, "success", map[string]any{"agent_id": AgentID, "tunnels": list, "pending": []any{}, "notices": []any{},
			"permissions": map[string]any{"is_self_managed": true, "has_premium": f.Premium, "account_status": "verified"}})
	case "/v1/tunnels/create":
		var in createReq
		if !strict(r, &in) || in.Name == nil || in.Protocol == nil || in.Origin == nil || in.Origin.Data == nil ||
			in.Endpoint == nil || in.Endpoint.Details == nil || in.Enabled == nil {
			badBody(w)
			return
		}
		var rp rawPorts
		d := json.NewDecoder(bytes.NewReader(in.Protocol.Details))
		d.DisallowUnknownFields()
		if in.Protocol.Type != "raw-ports" || d.Decode(&rp) != nil || rp.Count == nil ||
			(rp.PortType != "tcp" && rp.PortType != "udp" && rp.PortType != "both") ||
			in.Origin.Type != "agent" || in.Endpoint.Type != "region" {
			badBody(w)
			return
		}
		fail := ""
		switch {
		case !ascii(*in.Name):
			fail = "TunnelNameIsNotAscii"
		case len(*in.Name) < 3 || len(rp.Desc) < 3 || !ascii(rp.Desc):
			fail = "InvalidTunnelConfig"
		case in.Origin.Data.AgentID != AgentID:
			fail = "AgentNotFound"
		case in.Endpoint.Details.Region != "global":
			fail = "RegionRequiresPlayitPremium"
		case *rp.Count != 1:
			fail = "InvalidPortCount"
		}
		if fail != "" {
			send(w, "fail", fail)
			return
		}
		f.n++
		t := Tunnel{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.n), Name: *in.Name, Proto: rp.PortType, PublicPort: 30000 + f.n}
		if c := in.Origin.Data.Config; c != nil {
			for _, fld := range c.Fields {
				if fld.Name == "local_port" {
					t.LocalPort = fld.Value
				}
			}
		}
		f.Tunnels = append(f.Tunnels, t)
		send(w, "success", map[string]string{"id": t.ID})
	case "/tunnels/delete":
		var in struct {
			ID *string `json:"tunnel_id"`
		}
		if !strict(r, &in) || in.ID == nil || len(*in.ID) != 36 {
			badBody(w)
			return
		}
		for i, t := range f.Tunnels {
			if t.ID == *in.ID {
				f.Tunnels = append(f.Tunnels[:i], f.Tunnels[i+1:]...)
				send(w, "success", nil)
				return
			}
		}
		send(w, "fail", "TunnelNotFound")
	case "/v1/tunnels/list":
		list := []any{}
		for _, t := range f.Tunnels {
			src := map[string]string{"resource": "port-allocation", "id": t.ID}
			list = append(list, map[string]any{"id": t.ID, "name": t.Name, "connect_addresses": []any{
				map[string]any{"type": "ip4", "value": map[string]any{"address": "147.185.221.20", "default_port": t.PublicPort, "source": src}},
				map[string]any{"type": "addr4", "value": map[string]any{"address": fmt.Sprintf("147.185.221.20:%d", t.PublicPort), "source": src}},
				map[string]any{"type": "auto", "value": map[string]any{"address": fmt.Sprintf("test-name.gl.at.ply.gg:%d", t.PublicPort), "source": src}},
			}})
		}
		send(w, "success", map[string]any{"tunnels": list})
	default:
		w.WriteHeader(404)
		send(w, "error", map[string]any{"type": "path-not-found", "message": map[string]string{"path": path}})
	}
}

// claim: the first poll says nobody visited yet, the second that the user
// must approve, after that it is accepted.
func (f *Fake) claim(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/claim/setup":
		var in struct {
			Code    *string `json:"code"`
			Type    *string `json:"agent_type"`
			Version *string `json:"version"`
		}
		if !strict(r, &in) || in.Code == nil || in.Type == nil || in.Version == nil ||
			(*in.Type != "self-managed" && *in.Type != "assignable") {
			badBody(w)
			return
		}
		f.Claims[*in.Code]++
		states := []string{"", "WaitingForUserVisit", "WaitingForUser"}
		if n := f.Claims[*in.Code]; n < len(states) {
			send(w, "success", states[n])
			return
		}
		send(w, "success", "UserAccepted")
	case "/claim/exchange":
		var in struct {
			Code *string `json:"code"`
		}
		if !strict(r, &in) || in.Code == nil {
			badBody(w)
			return
		}
		if f.Claims[*in.Code] < 3 {
			send(w, "fail", "NotAccepted")
			return
		}
		send(w, "success", map[string]string{"secret_key": Secret})
	default:
		w.WriteHeader(404)
	}
}
