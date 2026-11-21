// Package playit runs playit.gg's official agent and creates tunnels through
// playit's API, the same calls playit's own agent and CLI make
// (github.com/playit-cloud/playit-agent, BSD-2-Clause).
package playit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
)

var (
	// APIBase can be pointed elsewhere with ZEROOS_PLAYIT_API (for tests).
	APIBase    = envOr("ZEROOS_PLAYIT_API", "https://api.playit.gg")
	AgentImage = "ghcr.io/playit-cloud/playit-agent:latest"
)

const containerName = "zeroos-playit"

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type Playit struct {
	c      *core.Core
	client *http.Client
	claim  sync.Mutex // one claim check at a time

	mu      sync.Mutex
	asked   time.Time // when playit last answered
	last    Info      // that answer
	premium bool      // as of that answer
	secure  []Secure  // HTTPS tunnels made on playit.gg for this computer
}

// Secure is an HTTPS tunnel that was made on playit.gg and ends at this
// computer's agent. playit only lets a person make these, so ZeroOS finds
// them and matches them to an app by the local port they point at.
type Secure struct {
	TunnelID string `json:"-"`
	Name     string `json:"name"`
	Host     string `json:"host"`    // e.g. films.playit.plus
	Port     int    `json:"-"`       // local port requests are sent to
	Cert     bool   `json:"cert"`    // playit serves the certificate (TLS Termination = letsencrypt)
	Problem  string `json:"problem"` // why playit has it switched off, if it does
}

// URL is the address people type.
func (x Secure) URL() string { return "https://" + x.Host }

// SecureFor returns the HTTPS tunnel that points at a local port, if one was found.
func (p *Playit) SecureFor(port int) *Secure {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.secure {
		if p.secure[i].Port == port && port > 0 {
			x := p.secure[i]
			return &x
		}
	}
	return nil
}

// Premium tells whether the linked account had Premium the last time playit answered.
func (p *Playit) Premium() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.premium
}

// Refresh asks playit again which tunnels this computer has.
func (p *Playit) Refresh() { _ = p.Info() }

// InfoCached is Info without asking playit more than once every 20 seconds.
// Open windows ask every few seconds to stay current; playit shouldn't be.
func (p *Playit) InfoCached() Info {
	p.mu.Lock()
	fresh, last := time.Since(p.asked) < 20*time.Second, p.last
	p.mu.Unlock()
	if !fresh || last.Linked != p.Linked() {
		return p.Info()
	}
	last.AgentRunning = AgentRunning()
	return last
}

// forget makes the next InfoCached ask playit again (after something changed there).
func (p *Playit) forget() {
	p.mu.Lock()
	p.asked = time.Time{}
	p.mu.Unlock()
}

// RefreshIfStale does the same unless playit answered within the last 20 seconds.
func (p *Playit) RefreshIfStale() {
	p.mu.Lock()
	fresh := time.Since(p.asked) < 20*time.Second
	p.mu.Unlock()
	if !fresh {
		p.Refresh()
	}
}

func New(c *core.Core) *Playit {
	return &Playit{c: c, client: &http.Client{Timeout: 20 * time.Second}}
}

type apiResult struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

func (p *Playit) call(path string, body any, secret string, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", APIBase+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Agent-Key "+strings.TrimSpace(secret))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return errors.New("Couldn't reach playit.gg. Check that this computer is online.")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 429 {
		return errors.New("playit.gg asked us to slow down. Try again in a minute.")
	}
	var r apiResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("playit.gg sent an unexpected answer (HTTP %d)", resp.StatusCode)
	}
	if r.Status != "success" {
		log.Printf("playit: %s answered %s: %s", path, r.Status, string(r.Data))
		return &FailError{Status: r.Status, Data: string(r.Data)}
	}
	if out != nil {
		return json.Unmarshal(r.Data, out)
	}
	return nil
}

// FailError is playit's "fail"/"error" answer.
type FailError struct {
	Status string
	Data   string
}

func (e *FailError) Error() string {
	d := strings.Trim(e.Data, `"`)
	var detail struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(e.Data), &detail) == nil && detail.Message != "" {
		d = detail.Message
	}
	switch d {
	case "InvalidAgentKey":
		return "The playit key isn't valid any more. Link playit again."
	case "RequiresVerifiedAccount":
		return "Your playit account needs a verified email before it can make tunnels."
	case "RequiresPlayitPremium", "RegionRequiresPlayitPremium", "PublicPortRequiresPlayitPremium":
		return "playit.gg only allows this with Premium."
	case "AgentNotFound", "InvalidAgentId":
		return "playit.gg doesn't know this computer any more. Link playit again."
	case "AgentVersionTooOld":
		return "The playit agent is too old. Restart ZeroOS to fetch the new one."
	case "failed to parse body":
		return "playit.gg changed how tunnels are made and this ZeroOS doesn't know the new way yet. Update ZeroOS."
	case "TunnelLimitReached", "PortAllocationLimitReached":
		return "Your playit account has no tunnels left. Remove one, or upgrade to Premium."
	}
	return "playit.gg said: " + d
}

// --- linking ---------------------------------------------------------------

func (p *Playit) secret() string {
	s, _ := p.c.Decrypt(p.c.Setting("playit_secret"))
	return s
}

func (p *Playit) Linked() bool { return p.secret() != "" }

// StartClaim makes a claim code and returns the playit.gg link to approve it.
func (p *Playit) StartClaim() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	code := hex.EncodeToString(b)
	_ = p.c.SetSetting("playit_claim", code)
	return "https://playit.gg/claim/" + code
}

// PollClaim checks whether the claim was approved, and finishes linking if so.
// Returns a state: waiting, approve, linked, rejected.
func (p *Playit) PollClaim() (string, error) {
	p.claim.Lock()
	defer p.claim.Unlock()
	code := p.c.Setting("playit_claim")
	if code == "" {
		if p.Linked() {
			return "linked", nil
		}
		return "", errors.New("Start linking first.")
	}
	var state string
	err := p.call("/claim/setup", map[string]any{"code": code, "agent_type": "self-managed", "version": "zeroos " + core.Version}, "", &state)
	if err != nil {
		return "", err
	}
	switch state {
	case "WaitingForUserVisit":
		return "waiting", nil
	case "WaitingForUser":
		return "approve", nil
	case "UserRejected":
		_ = p.c.SetSetting("playit_claim", "")
		return "rejected", nil
	case "UserAccepted":
		var key struct {
			SecretKey string `json:"secret_key"`
		}
		if err := p.call("/claim/exchange", map[string]any{"code": code}, "", &key); err != nil {
			return "approve", nil // not ready yet
		}
		// The key is what links the account. If the agent can't start right
		// now, that shows as "Agent not running" and is retried later.
		if err := p.SetSecret(key.SecretKey); err != nil && !p.Linked() {
			return "", err
		}
		_ = p.c.SetSetting("playit_claim", "")
		return "linked", nil
	}
	return "waiting", nil
}

// SetSecret stores an agent secret key (also used for "paste a key" linking) and starts the agent.
func (p *Playit) SetSecret(secret string) error {
	defer p.forget()
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return errors.New("The secret key is empty.")
	}
	enc, err := p.c.Encrypt(secret)
	if err != nil {
		return err
	}
	if err := p.c.SetSetting("playit_secret", enc); err != nil {
		return err
	}
	return p.StartAgent()
}

// Unlink stops the agent and forgets the key. Tunnels stay on the playit account.
func (p *Playit) Unlink() error {
	defer p.forget()
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	_ = p.c.SetSetting("playit_secret", "")
	p.mu.Lock()
	p.premium, p.secure = false, nil
	p.mu.Unlock()
	_, err := p.c.DB.Exec(`UPDATE tunnels SET playit_id='', address=''`)
	return err
}

// --- the agent container -----------------------------------------------------

// certDir is where the agent keeps the certificates of HTTPS tunnels, so they
// survive restarts and updates (Let's Encrypt only hands out a few per week).
func (p *Playit) certDir() string { return filepath.Join(p.c.Cfg.DataDir, "playit") }

// StartAgent (re)creates the official agent container with host networking.
func (p *Playit) StartAgent() error {
	s := p.secret()
	if s == "" {
		return nil
	}
	if err := os.MkdirAll(p.certDir(), 0o700); err != nil {
		return err
	}
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", containerName, "--restart", "unless-stopped",
		"--net=host", "--label", "zeroos.system=playit", "-v", p.certDir()+":/var/lib/playit",
		"-e", "SECRET_KEY="+s, AgentImage).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Couldn't start the playit agent: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureAgent starts the agent when it isn't running, and replaces one made
// by an older ZeroOS that had nowhere to keep certificates.
func (p *Playit) EnsureAgent() {
	if !p.Linked() {
		return
	}
	if AgentRunning() {
		out, err := exec.Command("docker", "inspect", "-f", "{{range .Mounts}}{{.Source}}\n{{end}}", containerName).Output()
		if err != nil || strings.Contains("\n"+string(out), "\n"+p.certDir()+"\n") {
			return
		}
	}
	if err := p.StartAgent(); err != nil {
		log.Printf("playit: %v", err)
	}
}

// AgentRunning tells whether the agent container runs.
func AgentRunning() bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", containerName).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// --- account info & tunnels ------------------------------------------------

type Info struct {
	Linked       bool   `json:"linked"`
	AgentRunning bool   `json:"agentRunning"`
	Premium      bool   `json:"premium"`
	AgentID      string `json:"agentId,omitempty"`
	Problem      string `json:"problem,omitempty"`
}

type runData struct {
	AgentID string `json:"agent_id"`
	Tunnels []struct {
		ID      string  `json:"id"`
		Name    string  `json:"name"`
		Address string  `json:"display_address"`
		Type    *string `json:"tunnel_type"`
		Config  struct {
			Fields []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"fields"`
		} `json:"agent_config"`
		Disabled *string `json:"disabled_reason"`
	} `json:"tunnels"`
	Permissions struct {
		HasPremium bool `json:"has_premium"`
	} `json:"permissions"`
}

// hostOf takes the name out of an address like "https://films.playit.plus:443/".
// It is the same name the playit agent asks a certificate for.
func hostOf(addr string) string {
	h := strings.TrimSpace(addr)
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h, "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			h = h[:i]
		}
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if h == "" || !strings.Contains(h, ".") || strings.ContainsAny(h, " []") {
		return ""
	}
	return h
}

// secureTunnels picks the HTTPS tunnels out of what playit says this agent runs.
func secureTunnels(rd runData) []Secure {
	out := []Secure{}
	for _, t := range rd.Tunnels {
		if t.Type == nil || *t.Type != "https" {
			continue
		}
		x := Secure{TunnelID: t.ID, Name: t.Name, Host: hostOf(t.Address)}
		var httpPort, httpsPort int
		for _, f := range t.Config.Fields {
			switch f.Name {
			case "http_port":
				httpPort, _ = strconv.Atoi(f.Value)
			case "https_port":
				httpsPort, _ = strconv.Atoi(f.Value)
			case "tls_termination":
				x.Cert = f.Value == "letsencrypt"
			}
		}
		// Like the agent: requests go to the HTTP port, or to the HTTPS port when that is 0.
		x.Port = httpPort
		if x.Port == 0 {
			x.Port = httpsPort
		}
		if t.Disabled != nil {
			x.Problem = *t.Disabled
		}
		if x.Host == "" {
			log.Printf("playit: HTTPS tunnel %q has an address ZeroOS can't read: %q", t.Name, t.Address)
			continue
		}
		out = append(out, x)
	}
	return out
}

func (p *Playit) Info() Info {
	in := Info{Linked: p.Linked()}
	if !in.Linked {
		p.mu.Lock()
		p.premium, p.secure = false, nil
		p.mu.Unlock()
		return in
	}
	in.AgentRunning = AgentRunning()
	var rd runData
	if err := p.call("/v1/agents/rundata", map[string]any{}, p.secret(), &rd); err != nil {
		in.Problem = err.Error()
		in.Premium = p.Premium()
		return in
	}
	in.AgentID, in.Premium = rd.AgentID, rd.Permissions.HasPremium
	p.mu.Lock()
	p.premium, p.secure, p.asked, p.last = in.Premium, secureTunnels(rd), time.Now(), in
	p.mu.Unlock()
	return in
}

// asciiName makes a name playit accepts: plain ASCII, 3 to 60 characters.
func asciiName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 32 && r < 127 {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	n := strings.Join(strings.Fields(b.String()), " ")
	if len(n) > 60 {
		n = strings.TrimSpace(n[:60])
	}
	for len(n) < 3 {
		n += "-"
	}
	return n
}

// tunnelRequest is the body of /v1/tunnels/create, in the shape playit.gg's
// own website sends it: what to carry (protocol), where it ends up (origin:
// this computer's agent and a local port) and where people connect (endpoint:
// "global" is playit's free network).
func tunnelRequest(name, agentID string, localPort int, proto string) map[string]any {
	if proto != "udp" && proto != "both" {
		proto = "tcp"
	}
	name = asciiName(name)
	return map[string]any{
		"name": name,
		"protocol": map[string]any{"type": "raw-ports", "details": map[string]any{
			"port_type":            proto,
			"port_count":           1,
			"software_description": name,
		}},
		"origin": map[string]any{"type": "agent", "data": map[string]any{
			"agent_id": agentID,
			"config": map[string]any{"fields": []map[string]string{
				{"name": "local_ip", "value": "127.0.0.1"},
				{"name": "local_port", "value": fmt.Sprint(localPort)},
			}},
		}},
		"endpoint": map[string]any{"type": "region", "details": map[string]any{"region": "global", "port": nil}},
		"enabled":  true,
	}
}

// CreateTunnel makes a playit tunnel to a local port and returns its id.
func (p *Playit) CreateTunnel(name string, localPort int, proto string) (string, error) {
	defer p.forget()
	if !p.Linked() {
		return "", errors.New("Link a playit account first (Settings › Sharing online).")
	}
	var rd runData
	if err := p.call("/v1/agents/rundata", map[string]any{}, p.secret(), &rd); err != nil {
		return "", err
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := p.call("/v1/tunnels/create", tunnelRequest(name, rd.AgentID, localPort, proto), p.secret(), &res); err != nil {
		return "", err
	}
	if res.ID == "" {
		return "", errors.New("playit.gg made the tunnel but didn't say which one. Look at playit.gg, then try again.")
	}
	return res.ID, nil
}

// DeleteTunnel removes a tunnel from the playit account.
func (p *Playit) DeleteTunnel(id string) error {
	defer p.forget()
	if id == "" || !p.Linked() {
		return nil
	}
	err := p.call("/tunnels/delete", map[string]any{"tunnel_id": id}, p.secret(), nil)
	var fe *FailError
	if errors.As(err, &fe) && strings.Contains(fe.Data, "TunnelNotFound") {
		return nil // already removed on playit.gg
	}
	return err
}

// Addresses returns the public address of each tunnel on the account.
func (p *Playit) Addresses() (map[string]string, error) {
	_, addrs, err := p.Tunnels()
	return addrs, err
}

// Tunnels lists the tunnels on the account: which exist, and the public
// address of each that has one already.
func (p *Playit) Tunnels() (exist map[string]bool, addrs map[string]string, err error) {
	var res struct {
		Tunnels []struct {
			ID       string `json:"id"`
			Connects []struct {
				Type  string `json:"type"`
				Value struct {
					Address string `json:"address"`
				} `json:"value"`
			} `json:"connect_addresses"`
		} `json:"tunnels"`
	}
	if err := p.call("/v1/tunnels/list", map[string]any{}, p.secret(), &res); err != nil {
		return nil, nil, err
	}
	out, exist := map[string]string{}, map[string]bool{}
	for _, t := range res.Tunnels {
		exist[t.ID] = true
		// a name with a port ("x.gl.joinmc.link:1234") is the one to hand out;
		// "ip4"/"ip6" entries carry no port.
		best := 99
		for _, c := range t.Connects {
			rank, ok := map[string]int{"domain": 0, "auto": 1, "addr4": 2, "addr6": 3}[c.Type]
			if ok && c.Value.Address != "" && rank < best {
				best, out[t.ID] = rank, c.Value.Address
			}
		}
	}
	return exist, out, nil
}
