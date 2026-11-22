// ZeroOS: run Docker apps on a Debian mini PC, with storage on your NAS and playit.gg built in.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MightyX3N/zeroos/internal/apps"
	"github.com/MightyX3N/zeroos/internal/auth"
	"github.com/MightyX3N/zeroos/internal/core"
	"github.com/MightyX3N/zeroos/internal/playit"
	"github.com/MightyX3N/zeroos/internal/server"
	"github.com/MightyX3N/zeroos/internal/storage"
	"github.com/MightyX3N/zeroos/web"
)

const usage = `ZeroOS %s

Usage: zeroos <command>

  serve                     Run ZeroOS (the system service does this)
  status                    Show web address, apps, NAS shares and playit
  web-address               Print every address the web UI is reachable on
  setup-code                Show the setup code for a new install
  apps                      List apps
  apps start|stop|restart <app>
  apps logs <app>           Show an app's latest output
  reset-password <user>     Give a user a new password (and turn off two-step sign-in)
  doctor                    Check Docker, NAS shares, playit and disk space
  update                    Update ZeroOS to the newest release
  version                   Print the version
`

func main() {
	cfg := core.DefaultConfig()
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Printf(usage, core.Version)
		return
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve(cfg)
	case "status":
		err = status(cfg)
	case "web-address":
		err = webAddress(cfg)
	case "setup-code":
		err = setupCode(cfg)
	case "apps":
		err = appsCmd(cfg, args[1:])
	case "reset-password":
		err = resetPassword(cfg, args[1:])
	case "doctor":
		doctor(cfg)
	case "update":
		err = update()
	case "verify":
		err = verify(cfg, args[1:])
	case "verify-merge":
		err = verifyMerge(args[1:])
	case "version", "--version", "-v":
		fmt.Println(core.Version)
	case "help", "--help", "-h":
		fmt.Printf(usage, core.Version)
	default:
		fmt.Printf(usage, core.Version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
}

func serve(cfg core.Config) error {
	c, err := core.Open(cfg)
	if err != nil {
		return err
	}
	a, err := auth.New(c)
	if err != nil {
		return err
	}
	st := storage.New(c)
	cat := apps.NewCatalog(c)
	mgr := apps.NewManager(c, cat, st)
	p := playit.New(c)
	srv := server.New(c, a, cat, mgr, st, p, web.FS())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
}

type statusData struct {
	Version   string   `json:"version"`
	Addresses []string `json:"addresses"`
	SetupDone bool     `json:"setupDone"`
	Apps      []struct {
		ID, Name, State, Problem string
	} `json:"apps"`
	Shares []struct {
		Name      string `json:"name"`
		Host      string `json:"host"`
		Share     string `json:"share"`
		Connected bool   `json:"connected"`
		Problem   string `json:"problem"`
	} `json:"shares"`
	Playit struct {
		Linked       bool   `json:"linked"`
		AgentRunning bool   `json:"agentRunning"`
		Problem      string `json:"problem"`
	} `json:"playit"`
	System struct {
		CPU      float64 `json:"cpu"`
		MemTotal uint64  `json:"memTotal"`
		MemUsed  uint64  `json:"memUsed"`
		SSD      struct {
			Total, Free uint64
		} `json:"ssd"`
	} `json:"system"`
}

func getStatus(cfg core.Config) (*statusData, error) {
	var st statusData
	if err := server.SocketCall(cfg.Socket, "GET", "/status", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func gb(n uint64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

func status(cfg core.Config) error {
	st, err := getStatus(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("ZeroOS %s is running\n\n", st.Version)
	fmt.Println("Web UI:")
	for _, a := range st.Addresses {
		fmt.Println("  " + a)
	}
	if !st.SetupDone {
		fmt.Println("  Not set up yet. Get the setup code with: sudo zeroos setup-code")
	}
	fmt.Printf("\nThis mini PC: CPU %.0f%%, memory %s of %s, SSD %s free\n", st.System.CPU, gb(st.System.MemUsed), gb(st.System.MemTotal), gb(st.System.SSD.Free))
	fmt.Println("\nNAS shares:")
	if len(st.Shares) == 0 {
		fmt.Println("  none")
	}
	for _, s := range st.Shares {
		state := "connected"
		if !s.Connected {
			state = "NOT connected " + s.Problem
		}
		fmt.Printf("  %-16s //%s/%s  %s\n", s.Name, s.Host, s.Share, state)
	}
	fmt.Println("\nApps:")
	if len(st.Apps) == 0 {
		fmt.Println("  none")
	}
	for _, a := range st.Apps {
		fmt.Printf("  %-20s %-10s %s\n", a.ID, a.State, a.Problem)
	}
	fmt.Print("\nplayit: ")
	switch {
	case !st.Playit.Linked:
		fmt.Println("not linked")
	case st.Playit.AgentRunning:
		fmt.Println("linked, agent running")
	default:
		fmt.Println("linked, agent NOT running " + st.Playit.Problem)
	}
	return nil
}

func webAddress(cfg core.Config) error {
	st, err := getStatus(cfg)
	if err != nil {
		return err
	}
	for _, a := range st.Addresses {
		fmt.Println(a)
	}
	return nil
}

func setupCode(cfg core.Config) error {
	b, err := os.ReadFile(cfg.DataDir + "/setup-code")
	if err != nil {
		return errors.New("There's no setup code: ZeroOS is already set up (or run this with sudo).")
	}
	fmt.Println("Setup code:", strings.TrimSpace(string(b)))
	return nil
}

func appsCmd(cfg core.Config, args []string) error {
	if len(args) == 0 {
		st, err := getStatus(cfg)
		if err != nil {
			return err
		}
		if len(st.Apps) == 0 {
			fmt.Println("No apps yet.")
		}
		for _, a := range st.Apps {
			fmt.Printf("%-20s %-10s %s\n", a.ID, a.State, a.Problem)
		}
		return nil
	}
	if len(args) < 2 {
		return errors.New("Which app? For example: zeroos apps restart jellyfin")
	}
	switch args[0] {
	case "start", "stop", "restart":
		var out map[string]bool
		if err := server.SocketCall(cfg.Socket, "POST", "/apps/"+args[1]+"/"+args[0], &out); err != nil {
			return err
		}
		fmt.Println("✓ Done")
	case "logs":
		var out struct{ Log string }
		if err := server.SocketCall(cfg.Socket, "GET", "/apps/"+args[1]+"/logs", &out); err != nil {
			return err
		}
		fmt.Print(out.Log)
	default:
		return errors.New("Use start, stop, restart or logs.")
	}
	return nil
}

func resetPassword(cfg core.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("Which user? For example: sudo zeroos reset-password admin")
	}
	if os.Geteuid() != 0 {
		return errors.New("Run this with sudo.")
	}
	c, err := core.Open(cfg)
	if err != nil {
		return err
	}
	a, err := auth.New(c)
	if err != nil {
		return err
	}
	u, err := a.UserByName(args[0])
	if err != nil {
		return fmt.Errorf("There's no user called %q.", args[0])
	}
	pw := core.RandomString(14)
	if err := a.SetPassword(u.ID, pw); err != nil {
		return err
	}
	_ = a.DisableTOTP(u.ID)
	c.Log("command line", "reset the password of", u.Username, "", true, "")
	fmt.Printf("New password for %s: %s\nSign in with it, then change it under Settings › Profile.\n", u.Username, pw)
	return nil
}

func doctor(cfg core.Config) {
	check := func(ok bool, good, bad string) {
		if ok {
			fmt.Println("✓", good)
		} else {
			fmt.Println("✗", bad)
		}
	}
	err := exec.Command("docker", "info").Run()
	check(err == nil, "Docker is running", "Docker isn't running. Try: sudo systemctl start docker")
	err = exec.Command("docker", "compose", "version").Run()
	check(err == nil, "Docker Compose is installed", "Docker Compose is missing. Run the ZeroOS installer again.")
	_, err = exec.LookPath("mount.cifs")
	check(err == nil, "SMB tools for NAS shares are installed", "SMB tools (cifs-utils) are missing: NAS shares can't connect. Run: sudo apt install cifs-utils")
	if u, err := storage.UsageOf(cfg.DataDir); err == nil {
		check(u.Free > 10e9, fmt.Sprintf("SSD has %s free", gb(u.Free)), fmt.Sprintf("SSD is almost full (%s free). Apps may stop working.", gb(u.Free)))
	}
	st, err := getStatus(cfg)
	check(err == nil, "ZeroOS is running", "ZeroOS isn't running. Try: sudo systemctl restart zeroos")
	if err != nil {
		return
	}
	for _, s := range st.Shares {
		check(s.Connected, "NAS share "+s.Name+" is connected", "NAS share "+s.Name+" isn't connected. "+s.Problem+" Check that the NAS is on.")
	}
	for _, a := range st.Apps {
		if a.State == "crashed" || a.State == "failed" {
			fmt.Printf("✗ App %s: %s. See: sudo zeroos apps logs %s\n", a.ID, a.Problem, a.ID)
		}
	}
	if st.Playit.Linked {
		check(st.Playit.AgentRunning, "playit agent is running", "playit agent isn't running. "+st.Playit.Problem)
	}
}

// update replaces this program with the newest GitHub release, after checking its checksum.
func update() error {
	if os.Geteuid() != 0 {
		return errors.New("Run this with sudo.")
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get("https://api.github.com/repos/MightyX3N/ZeroOS/releases/latest")
	if err != nil {
		return errors.New("Couldn't reach GitHub.")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return errors.New("There are no ZeroOS releases yet.")
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return err
	}
	if rel.Tag == core.Version || "v"+core.Version == rel.Tag {
		fmt.Println("✓ ZeroOS is up to date (" + core.Version + ")")
		return nil
	}
	want := "zeroos-linux-" + runtime.GOARCH
	var binURL, sumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			binURL = a.URL
		case "checksums.txt":
			sumURL = a.URL
		}
	}
	if binURL == "" || sumURL == "" {
		return errors.New("The newest release has no build for this computer.")
	}
	sums, err := fetch(client, sumURL)
	if err != nil {
		return err
	}
	bin, err := fetch(client, binURL)
	if err != nil {
		return err
	}
	h := sha256.Sum256(bin)
	got := hex.EncodeToString(h[:])
	okSum := false
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == want && f[0] == got {
			okSum = true
		}
	}
	if !okSum {
		return errors.New("The download didn't match its checksum, so it wasn't installed.")
	}
	self, _ := os.Executable()
	tmp := self + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, self); err != nil {
		return err
	}
	fmt.Println("✓ Updated to", rel.Tag, "- restarting ZeroOS. Apps keep running.")
	return exec.Command("systemctl", "restart", "zeroos").Run()
}

func fetch(c *http.Client, url string) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download failed (HTTP %d)", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// verify runs the install test behind the Verified badge. It is meant for a
// throwaway machine (ZeroOS's GitHub workflow): it installs and removes apps.
//
//	zeroos verify --out results.json [--shard 0/10] [--only a,b] [--limit 5] [--prune]
func verify(cfg core.Config, args []string) error {
	out, only, shard, of, limit, prune := "verify-results.json", []string(nil), 0, 1, 0, false
	for i := 0; i < len(args); i++ {
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch args[i] {
		case "--out":
			out = next()
		case "--only":
			only = strings.Split(next(), ",")
		case "--shard":
			if _, err := fmt.Sscanf(next(), "%d/%d", &shard, &of); err != nil || of < 1 || shard < 0 || shard >= of {
				return errors.New("--shard wants something like 0/10")
			}
		case "--limit":
			fmt.Sscan(next(), &limit)
		case "--prune":
			prune = true
		default:
			return errors.New("unknown option " + args[i])
		}
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		return errors.New("Docker isn't running here, so nothing can be tested")
	}
	c, err := core.Open(cfg)
	if err != nil {
		return err
	}
	cat := apps.NewCatalog(c)
	if err := cat.Sync(); err != nil {
		return err
	}
	mgr := apps.NewManager(c, cat, storage.New(c))
	ids := cat.VerifyList(shard, of, only)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	fmt.Printf("Testing %d of %d store apps\n", len(ids), cat.Count())
	results := []apps.VerifyResult{}
	save := func() error {
		b, _ := json.MarshalIndent(map[string]any{"at": time.Now().Unix(), "zeroos": core.Version, "results": results}, "", "  ")
		return os.WriteFile(out, b, 0o644)
	}
	passed := 0
	for i, id := range ids {
		r := mgr.Verify(id)
		results = append(results, r)
		mark := "✗"
		if r.OK {
			mark = "✓"
			passed++
		} else if r.Skipped {
			mark = "–"
		}
		fmt.Printf("[%d/%d] %s %s (%ds) %s\n", i+1, len(ids), mark, r.ID, r.Seconds, r.Why)
		if err := save(); err != nil {
			return err
		}
		if prune { // a test machine's disk is small
			_ = exec.Command("docker", "system", "prune", "-af", "--volumes").Run()
		}
	}
	fmt.Printf("%d of %d passed\n", passed, len(ids))
	return save()
}

// verifyMerge folds result files into the published list.
//
//	zeroos verify-merge --prev verified.json --out verified.json --report report.md results-*.json
func verifyMerge(args []string) error {
	prevPath, out, report := "", "verified.json", ""
	var files []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--prev", "--out", "--report":
			if i+1 >= len(args) {
				return errors.New(args[i] + " wants a file")
			}
			switch args[i] {
			case "--prev":
				prevPath = args[i+1]
			case "--out":
				out = args[i+1]
			default:
				report = args[i+1]
			}
			i++
		default:
			files = append(files, args[i])
		}
	}
	var prev apps.VerifiedFile
	if b, err := os.ReadFile(prevPath); err == nil {
		_ = json.Unmarshal(b, &prev)
	}
	var all []apps.VerifyResult
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var doc struct {
			Results []apps.VerifyResult `json:"results"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		all = append(all, doc.Results...)
	}
	if len(all) == 0 {
		return errors.New("no results to merge")
	}
	merged := apps.MergeVerified(prev, all, time.Now().Unix())
	b, _ := json.MarshalIndent(merged, "", " ")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		return err
	}
	passed := 0
	var failed []string
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	for _, r := range all {
		if r.OK {
			passed++
			continue
		}
		if r.Skipped {
			r.Catalog += ", not tested"
		}
		why := strings.ReplaceAll(r.Why, "|", "/")
		if len(why) > 300 {
			why = why[:300] + "…"
		}
		failed = append(failed, fmt.Sprintf("| %s | %s | %s | %s |", r.ID, r.Catalog, r.Version, strings.ReplaceAll(why, "\n", " ")))
	}
	fmt.Printf("%d tested, %d passed; %d apps are verified now\n", len(all), passed, len(merged.Apps))
	if report != "" {
		text := fmt.Sprintf("# Install test\n\n%s: %d apps tested, %d passed. %d apps are verified now.\n\n## Did not pass\n\n| App | Catalog | Version | Why |\n|---|---|---|---|\n%s\n",
			time.Now().UTC().Format("2 Jan 2006 15:04 UTC"), len(all), passed, len(merged.Apps), strings.Join(failed, "\n"))
		return os.WriteFile(report, []byte(text), 0o644)
	}
	return nil
}
