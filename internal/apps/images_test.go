package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseImage(t *testing.T) {
	for in, want := range map[string]imageRef{
		"redis":                        {"registry-1.docker.io", "library/redis", "latest"},
		"redis:7-alpine":               {"registry-1.docker.io", "library/redis", "7-alpine"},
		"jellyfin/jellyfin:10.9":       {"registry-1.docker.io", "jellyfin/jellyfin", "10.9"},
		"ghcr.io/someone/thing:latest": {"ghcr.io", "someone/thing", "latest"},
		"ghcr.io/pterodactyl/panel":    {"ghcr.io", "pterodactyl/panel", "latest"},
		"localhost:5000/me/app:dev":    {"localhost:5000", "me/app", "dev"},
	} {
		got, ok := parseImage(in)
		if !ok || got != want {
			t.Errorf("parseImage(%q) = %+v %v, want %+v", in, got, ok, want)
		}
	}
	if _, ok := parseImage("redis@sha256:abc"); ok {
		t.Error("an image pinned to exact contents never has a newer build")
	}
}

// A registry that wants the usual anonymous pass before it answers.
func TestRemoteDigest(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:me/app:pull" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"token":"pass-123"}`))
		case r.URL.Path == "/v2/me/app/manifests/latest" && r.Method == "HEAD":
			if r.Header.Get("Authorization") != "Bearer pass-123" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test"`)
				w.WriteHeader(401)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "image.index") {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := registryURL
	registryURL = func(string) string { return srv.URL }
	defer func() { registryURL = old }()

	got, err := remoteDigest(context.Background(), imageRef{Host: "example.test", Repo: "me/app", Tag: "latest"})
	if err != nil || got != digest {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := remoteDigest(context.Background(), imageRef{Host: "example.test", Repo: "me/other", Tag: "latest"}); err == nil {
		t.Error("an image that isn't there should be an error, not a digest")
	}
}

func TestStartProblem(t *testing.T) {
	for _, line := range []string{
		"Error response from daemon: driver failed programming external connectivity on endpoint x: Bind for 0.0.0.0:7359 failed: port is already allocated",
		"Error response from daemon: failed to set up container networking: driver failed programming external connectivity: failed to bind host port for 0.0.0.0:7359:172.18.0.2:7359/udp: address already in use",
	} {
		got := startProblem("Jellyfin", []string{"Container x Starting", line})
		if !strings.Contains(got, "port 7359") || !strings.Contains(got, "Jellyfin couldn't start") {
			t.Errorf("startProblem = %q", got)
		}
	}
	if got := startProblem("Jellyfin", []string{"no such image"}); got != "The app didn't start. Check its log." {
		t.Errorf("startProblem = %q", got)
	}
}
