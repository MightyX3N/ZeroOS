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
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
)

var (
	APIBase    = "https://api.playit.gg"
	AgentImage = "ghcr.io/playit-cloud/playit-agent:latest"
)

const containerName = "zeroos-playit"

type Playit struct {
	c      *core.Core
	client *http.Client
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
		if err := p.SetSecret(key.SecretKey); err != nil {
			return "", err
		}
		_ = p.c.SetSetting("playit_claim", "")
		return "linked", nil
	}
	return "waiting", nil
}

// SetSecret stores an agent secret key (also used for "paste a key" linking) and starts the agent.
func (p *Playit) SetSecret(secret string) error {
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
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	_ = p.c.SetSetting("playit_secret", "")
	_, err := p.c.DB.Exec(`UPDATE tunnels SET playit_id='', address=''`)
	return err
}

// --- the agent container -----------------------------------------------------

// StartAgent (re)creates the official agent container with host networking.
func (p *Playit) StartAgent() error {
	s := p.secret()
	if s == "" {
		return nil
	}
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", containerName, "--restart", "unless-stopped",
		"--net=host", "--label", "zeroos.system=playit", "-e", "SECRET_KEY="+s, AgentImage).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Couldn't start the playit agent: %s", strings.TrimSpace(string(out)))
	}
	return nil
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
	AgentID     string `json:"agent_id"`
	Permissions struct {
		HasPremium bool `json:"has_premium"`
	} `json:"permissions"`
}

func (p *Playit) Info() Info {
	in := Info{Linked: p.Linked()}
	if !in.Linked {
		return in
	}
	in.AgentRunning = AgentRunning()
	var rd runData
	if err := p.call("/v1/agents/rundata", map[string]any{}, p.secret(), &rd); err != nil {
		in.Problem = err.Error()
		return in
	}
	in.AgentID, in.Premium = rd.AgentID, rd.Permissions.HasPremium
	return in
}

// CreateTunnel makes a playit tunnel to a local port and returns its id.
func (p *Playit) CreateTunnel(name string, localPort int, proto string) (string, error) {
	if !p.Linked() {
		return "", errors.New("Link a playit account first (Settings › Sharing online).")
	}
	var rd runData
	if err := p.call("/v1/agents/rundata", map[string]any{}, p.secret(), &rd); err != nil {
		return "", err
	}
	kind := map[string]string{"tcp": "custom-tcp", "udp": "custom-udp", "both": "custom-both"}[proto]
	if kind == "" {
		kind = "custom-tcp"
	}
	req := map[string]any{
		"name":    name,
		"ports":   map[string]any{"type": kind, "details": 1},
		"enabled": true,
		"alloc":   nil,
		"origin": map[string]any{"type": "agent", "data": map[string]any{
			"agent_id": rd.AgentID,
			"config": map[string]any{"fields": []map[string]string{
				{"name": "local_ip", "value": "127.0.0.1"},
				{"name": "local_port", "value": fmt.Sprint(localPort)},
			}},
		}},
		"firewall_id": nil,
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := p.call("/v1/tunnels/create", req, p.secret(), &res); err != nil {
		return "", err
	}
	return res.ID, nil
}

// DeleteTunnel removes a tunnel from the playit account.
func (p *Playit) DeleteTunnel(id string) error {
	if id == "" || !p.Linked() {
		return nil
	}
	return p.call("/tunnels/delete", map[string]any{"tunnel_id": id}, p.secret(), nil)
}

// Addresses returns the public address of each tunnel on the account.
func (p *Playit) Addresses() (map[string]string, error) {
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
		return nil, err
	}
	out := map[string]string{}
	for _, t := range res.Tunnels {
		for _, c := range t.Connects {
			if c.Value.Address != "" {
				out[t.ID] = c.Value.Address
				break
			}
		}
	}
	return out, nil
}
