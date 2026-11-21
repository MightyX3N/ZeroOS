package playit

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MightyX3N/zeroos/internal/core"
	"github.com/MightyX3N/zeroos/internal/playit/playittest"
)

func setup(t *testing.T) (*Playit, *playittest.Fake) {
	t.Helper()
	fake := playittest.New()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	old := APIBase
	APIBase = srv.URL
	t.Cleanup(func() { APIBase = old })
	c, err := core.Open(core.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := New(c)
	enc, _ := c.Encrypt(playittest.Secret)
	_ = c.SetSetting("playit_secret", enc)
	return p, fake
}

func TestTunnels(t *testing.T) {
	p, fake := setup(t)
	// a name with characters playit refuses, and a very short one
	id, err := p.CreateTunnel("ZeroOS · Jellyfin ✨", 20001, "tcp")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id2, err := p.CreateTunnel("x", 25565, "both")
	if err != nil {
		t.Fatalf("create short name: %v", err)
	}
	if len(fake.Tunnels) != 2 || fake.Tunnels[0].LocalPort != "20001" || fake.Tunnels[0].Proto != "tcp" || fake.Tunnels[1].Proto != "both" {
		t.Fatalf("playit got %+v", fake.Tunnels)
	}
	addrs, err := p.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	if a := addrs[id]; a != "test-name.gl.at.ply.gg:30001" {
		t.Fatalf("address = %q, want the name with its port", a)
	}
	if err := p.DeleteTunnel(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := p.DeleteTunnel(id); err != nil {
		t.Fatalf("deleting one that is already gone should be fine: %v", err)
	}
	if err := p.DeleteTunnel(id2); err != nil || len(fake.Tunnels) != 0 {
		t.Fatalf("delete: %v, left %+v", err, fake.Tunnels)
	}
}

func TestClaim(t *testing.T) {
	p, _ := setup(t)
	_ = p.c.SetSetting("playit_secret", "")
	AgentImage = "" // no agent container in tests
	link := p.StartClaim()
	if !strings.HasPrefix(link, "https://playit.gg/claim/") {
		t.Fatal(link)
	}
	var states []string
	for i := 0; i < 3; i++ {
		s, err := p.PollClaim()
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	if states[0] != "waiting" || states[1] != "approve" || states[2] != "linked" {
		t.Fatalf("states %v", states)
	}
	if !p.Linked() {
		t.Fatalf("not linked after approval, states %v", states)
	}
}

func TestAsciiName(t *testing.T) {
	for in, want := range map[string]string{"ZeroOS - Jellyfin": "ZeroOS - Jellyfin", "Blåbær": "Bl-b-r", "a": "a--", "": "---"} {
		if got := asciiName(in); got != want {
			t.Errorf("asciiName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecureTunnels(t *testing.T) {
	p, fake := setup(t)
	if _, err := p.CreateTunnel("ZeroOS - Jellyfin", 20000, "tcp"); err != nil {
		t.Fatal(err)
	}
	p.Refresh()
	if p.Premium() || p.SecureFor(20000) != nil {
		t.Fatal("a plain tunnel is not a secure address")
	}
	fake.Premium = true
	fake.HTTPS = []playittest.HTTPS{
		{Name: "films", Host: "films.playit.plus", HTTPPort: 20000, Cert: true},
		{Name: "no cert", Host: "other.playit.plus", HTTPPort: 20001},
	}
	p.Refresh()
	x := p.SecureFor(20000)
	if !p.Premium() || x == nil || x.URL() != "https://films.playit.plus" || !x.Cert {
		t.Fatalf("got %+v", x)
	}
	if y := p.SecureFor(20001); y == nil || y.Cert {
		t.Fatalf("a tunnel without playit's certificate must say so: %+v", y)
	}
	if p.SecureFor(0) != nil || p.SecureFor(20002) != nil {
		t.Fatal("matched a port no tunnel points at")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"films.playit.plus": "films.playit.plus", "https://Films.playit.plus:443/": "films.playit.plus",
		"a.b.example.com:8443": "a.b.example.com", "localhost": "", "": "", "147.185.221.220:41260": "147.185.221.220",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
