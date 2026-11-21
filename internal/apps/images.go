package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Newer builds.
//
// Many apps are published under a name that moves, like ":latest": the name
// stays, what is behind it changes. The catalog entry then never changes, so
// ZeroOS asks the place the app is downloaded from whether the name points at
// something newer than what is on this computer. It only asks (one small
// request per image); nothing is downloaded until someone updates the app.

// imageRef is a Docker image name taken apart.
type imageRef struct {
	Host, Repo, Tag string
}

// parseImage understands names like "redis", "redis:7", "ghcr.io/owner/app:latest".
// ok is false for names pinned to exact contents (@sha256:...), which never move.
func parseImage(ref string) (r imageRef, ok bool) {
	if ref == "" || strings.Contains(ref, "@") {
		return r, false
	}
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	host, repo := "docker.io", name
	if i := strings.Index(name, "/"); i > 0 {
		first := name[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host, repo = first, name[i+1:]
		}
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	if repo == "" || tag == "" {
		return r, false
	}
	return imageRef{Host: host, Repo: repo, Tag: tag}, true
}

// registryURL is where a registry's API lives (a variable, so tests can stand in for one).
var registryURL = func(host string) string { return "https://" + host }

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// remoteDigest asks a registry what a name currently points at.
func remoteDigest(ctx context.Context, r imageRef) (string, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	target := registryURL(r.Host) + "/v2/" + r.Repo + "/manifests/" + r.Tag
	ask := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "HEAD", target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", manifestTypes)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return client.Do(req)
	}
	resp, err := ask("")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode == 401 {
		// public images still want a (free, anonymous) pass
		p := map[string]string{}
		for _, m := range authParam.FindAllStringSubmatch(resp.Header.Get("Www-Authenticate"), -1) {
			p[m[1]] = m[2]
		}
		if p["realm"] == "" {
			return "", errors.New("the registry wants a login")
		}
		q := url.Values{}
		if p["service"] != "" {
			q.Set("service", p["service"])
		}
		q.Set("scope", "repository:"+r.Repo+":pull")
		treq, err := http.NewRequestWithContext(ctx, "GET", p["realm"]+"?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		tresp, err := client.Do(treq)
		if err != nil {
			return "", err
		}
		var tok struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		err = json.NewDecoder(tresp.Body).Decode(&tok)
		tresp.Body.Close()
		if err != nil || tok.Token+tok.AccessToken == "" {
			return "", errors.New("the registry wants a login")
		}
		if tok.Token == "" {
			tok.Token = tok.AccessToken
		}
		if resp, err = ask(tok.Token); err != nil {
			return "", err
		}
		resp.Body.Close()
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("the registry answered %d", resp.StatusCode)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if !strings.HasPrefix(d, "sha256:") {
		return "", errors.New("the registry didn't say what the name points at")
	}
	return d, nil
}

// localDigests lists what a name pointed at when it was downloaded to this computer.
func localDigests(ref string) []string {
	out, err := exec.Command("docker", "image", "inspect", "--format", "{{json .RepoDigests}}", ref).Output()
	if err != nil {
		return nil
	}
	var list []string
	_ = json.Unmarshal(out, &list)
	var digests []string
	for _, x := range list {
		if i := strings.Index(x, "@"); i >= 0 {
			digests = append(digests, x[i+1:])
		}
	}
	return digests
}

// appImages lists the images an installed app runs.
func (m *Manager) appImages(id string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "compose", "-p", project(id), "--project-directory", m.appDir(id),
		"-f", filepath.Join(m.appDir(id), "docker-compose.yml"), "config", "--images").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// CheckBuild asks whether any of an app's images has a newer build to download.
func (m *Manager) CheckBuild(id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	newer := false
	for _, ref := range m.appImages(id) {
		r, ok := parseImage(ref)
		if !ok {
			continue
		}
		have := localDigests(ref)
		if len(have) == 0 {
			continue // not downloaded from a registry (or not at all): nothing to compare
		}
		d, err := remoteDigest(ctx, r)
		if err != nil {
			continue // offline, private, or the registry is having a day: ask again later
		}
		if !contains(have, d) {
			newer = true
		}
	}
	m.mu.Lock()
	m.newer[id] = newer
	m.mu.Unlock()
	return newer
}

// CheckBuilds does that for every installed app.
func (m *Manager) CheckBuilds() {
	list, err := m.List()
	if err != nil {
		return
	}
	for _, a := range list {
		if a.State == "installing" || a.State == "updating" || a.State == "removing" || a.State == "failed" {
			continue
		}
		m.CheckBuild(a.ID)
	}
}

func (m *Manager) newerBuild(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newer[id]
}

// --- keeping the images an update replaces ------------------------------------------

type keptImage struct {
	Ref  string `json:"ref"`  // the name the app uses
	Keep string `json:"keep"` // an extra name ZeroOS gave the old contents
}

func keepName(id string, i int) string { return fmt.Sprintf("zeroos-keep/%s:%d", id, i) }

// keepImages gives the app's current images a second name, so a newer
// download under the same name can't push them out.
func (m *Manager) keepImages(id string) {
	var kept []keptImage
	for i, ref := range m.appImages(id) {
		k := keepName(id, i)
		if exec.Command("docker", "tag", ref, k).Run() == nil {
			kept = append(kept, keptImage{Ref: ref, Keep: k})
		}
	}
	b, _ := json.Marshal(kept)
	_ = os.MkdirAll(m.undoDir(id), 0o755)
	_ = os.WriteFile(filepath.Join(m.undoDir(id), "images.json"), b, 0o644)
}

func (m *Manager) keptImages(id string) []keptImage {
	b, err := os.ReadFile(filepath.Join(m.undoDir(id), "images.json"))
	if err != nil {
		return nil
	}
	var kept []keptImage
	_ = json.Unmarshal(b, &kept)
	return kept
}

// restoreImages points the app's image names back at the kept contents.
func (m *Manager) restoreImages(id string) {
	for _, k := range m.keptImages(id) {
		_ = exec.Command("docker", "tag", k.Keep, k.Ref).Run()
	}
}

// dropKeptImages removes the extra names (the contents go when nothing else uses them).
func (m *Manager) dropKeptImages(id string) {
	for _, k := range m.keptImages(id) {
		_ = exec.Command("docker", "rmi", k.Keep).Run()
	}
}
