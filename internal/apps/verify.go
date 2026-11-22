package apps

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// The install test behind the "Verified" badge.
//
// An app is verified when ZeroOS itself installed it with the answers a person
// would give by just pressing Install, every part of it kept running, and its
// page answered. The test runs on GitHub for every app in the Store; the list
// of apps that passed is what a ZeroOS at home downloads with the catalogs.

// VerifyResult is the outcome of one app's install test.
type VerifyResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Catalog string `json:"catalog"`
	Version string `json:"version"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"` // couldn't be tested (its download failed); says nothing about the app
	Why     string `json:"why,omitempty"`
	Seconds int    `json:"seconds"`
}

// sampleAnswer fills in a question the way a first-time user plausibly would.
func sampleAnswer(f FormField) string {
	if f.Default != nil && fmt.Sprint(f.Default) != "" {
		return fmt.Sprint(f.Default)
	}
	if len(f.Options) > 0 {
		return fmt.Sprint(f.Options[0].Value)
	}
	if !f.Required {
		return ""
	}
	n := 0
	if f.Min != nil {
		n = *f.Min
	}
	switch f.Type {
	case "random":
		return ""
	case "boolean":
		return "false"
	case "number":
		if n < 1 {
			n = 1
		}
		return fmt.Sprint(n)
	case "email":
		return "test@example.com"
	case "url":
		return "http://example.com"
	case "fqdn", "fqdnip", "domain":
		return "example.com"
	case "ip":
		return "192.168.1.10"
	case "password":
		s := "Zeroos-test-1234"
		for len(s) < n {
			s += "x"
		}
		return s
	}
	s := "zeroos"
	for len(s) < n {
		s += "x"
	}
	if f.Max != nil && *f.Max > 0 && len(s) > *f.Max {
		s = s[:*f.Max]
	}
	return s
}

// Verify installs one store app, checks that it comes up, and removes it again.
func (m *Manager) Verify(storeID string) VerifyResult {
	start := time.Now()
	sa, ok := m.cat.Get(storeID)
	if !ok {
		return VerifyResult{ID: storeID, Why: "not in the store"}
	}
	res := VerifyResult{ID: sa.ID, Name: sa.Name, Catalog: sa.Catalog, Version: sa.Version}
	done := func(why string) VerifyResult {
		res.OK, res.Why, res.Seconds = why == "", why, int(time.Since(start).Seconds())
		return res
	}
	env := map[string]string{}
	for _, f := range sa.Form {
		if v := sampleAnswer(f); v != "" {
			env[f.EnvVariable] = v
		}
	}
	job, err := m.Install(InstallRequest{StoreID: sa.ID, Env: env, Folders: map[string]Picked{}}, 0)
	if err != nil {
		return done("install refused: " + err.Error())
	}
	id := job.AppID
	defer func() { _ = m.Remove(id, true) }()
	for end := time.Now().Add(20 * time.Minute); ; time.Sleep(2 * time.Second) {
		job.mu.Lock()
		finished, jerr, lines := job.Done, job.Error, append([]string(nil), job.Lines...)
		job.mu.Unlock()
		if finished {
			if jerr != "" {
				tail := lines
				if len(tail) > 6 {
					tail = tail[len(tail)-6:]
				}
				// A download that fails is the test machine's or the registry's trouble, not the app's.
				res.Skipped = strings.HasPrefix(jerr, "Couldn't download the app")
				return done(jerr + " | " + strings.Join(tail, " | "))
			}
			break
		}
		if time.Now().After(end) {
			return done("the install took longer than 20 minutes")
		}
	}
	a, err := m.Get(id)
	if err != nil {
		return done("gone after install")
	}
	if !m.comesUp(a) {
		if broken, total := brokenParts(id); total == 0 || broken > 0 {
			return done(fmt.Sprintf("%d of its %d parts don't keep running", broken, total))
		}
		return done("its page doesn't answer")
	}
	return done("")
}

// VerifyList returns the store apps to test: the downloaded catalogs (ZeroOS's
// own apps are made for it), in a fixed order, optionally one share of them.
func (cat *Catalog) VerifyList(shard, of int, only []string) []string {
	var ids []string
	for _, a := range cat.List("", "", false) {
		if a.Catalog == "zeroos" {
			continue
		}
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	if len(only) > 0 {
		want := map[string]bool{}
		for _, o := range only {
			want[strings.TrimSpace(o)] = true
		}
		var out []string
		for _, id := range ids {
			if want[id] {
				out = append(out, id)
			}
		}
		return out
	}
	if of <= 1 {
		return ids
	}
	var out []string
	for i, id := range ids {
		if i%of == shard {
			out = append(out, id)
		}
	}
	return out
}

// VerifiedFile is the list ZeroOS downloads: the apps that passed.
type VerifiedFile struct {
	At   int64                    `json:"at"`
	Apps map[string]VerifiedEntry `json:"apps"`
}

type VerifiedEntry struct {
	Version string `json:"version"`
	At      int64  `json:"at"`
}

// MergeVerified folds test results into the list: an app that passed is in, one
// that failed is out, one that wasn't tested this time stays as it was.
func MergeVerified(prev VerifiedFile, results []VerifyResult, at int64) VerifiedFile {
	out := VerifiedFile{At: at, Apps: map[string]VerifiedEntry{}}
	for id, e := range prev.Apps {
		out.Apps[id] = e
	}
	for _, r := range results {
		if r.Skipped {
			continue
		}
		if r.OK {
			out.Apps[r.ID] = VerifiedEntry{Version: r.Version, At: at}
		} else {
			delete(out.Apps, r.ID)
		}
	}
	return out
}

// VerifiedURL is where the list of apps that passed is published.
var VerifiedURL = "https://raw.githubusercontent.com/MightyX3N/ZeroOS/verified/verified.json"

func (cat *Catalog) verifiedPath() string { return cat.c.Cfg.StoreDir() + "/verified.json" }

// syncVerified downloads the newest list. Without it the last one stays.
func (cat *Catalog) syncVerified() error {
	src := VerifiedURL
	if v := os.Getenv("ZEROOS_VERIFIED_URL"); v != "" {
		src = v
	}
	if src == "off" {
		return nil
	}
	var b []byte
	var err error
	if strings.HasPrefix(src, "file://") {
		b, err = os.ReadFile(strings.TrimPrefix(src, "file://"))
	} else {
		b, err = httpGet(src, 4<<20, time.Minute)
	}
	if err != nil {
		return fmt.Errorf("couldn't download the list of verified apps: %w", err)
	}
	var f VerifiedFile
	if json.Unmarshal(b, &f) != nil || f.Apps == nil {
		return fmt.Errorf("the list of verified apps can't be read")
	}
	tmp := cat.verifiedPath() + ".part"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cat.verifiedPath())
}

// loadVerified reads the downloaded list, on top of what ZeroOS was built with.
func (cat *Catalog) loadVerified() map[string]bool {
	out := map[string]bool{}
	var builtIn []string
	_ = json.Unmarshal(verifiedJSON, &builtIn)
	for _, id := range builtIn {
		out[id] = true
	}
	if b, err := os.ReadFile(cat.verifiedPath()); err == nil {
		var f VerifiedFile
		if json.Unmarshal(b, &f) == nil {
			for id := range f.Apps {
				out[id] = true
			}
		}
	}
	return out
}
