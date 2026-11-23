package apps

import (
	"strings"
	"testing"

	"github.com/MightyX3N/zeroos/internal/core"
)

// ZeroOS's own apps load into the store and turn into a compose file.
func TestBuiltinPterodactyl(t *testing.T) {
	c, err := core.Open(core.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cat := NewCatalog(c)
	a, ok := cat.Get("pterodactyl")
	if !ok {
		t.Fatal("Pterodactyl isn't in the store")
	}
	if a.Catalog != "zeroos" || !a.Verified || !a.UsesDocker {
		t.Fatalf("catalog %q verified %v usesDocker %v", a.Catalog, a.Verified, a.UsesDocker)
	}
	if cat.LogoPath("pterodactyl") == "" {
		t.Error("no icon")
	}
	for _, h := range []string{"setup", "stop", "remove"} {
		if len(cat.Hook(a, h)) == 0 {
			t.Errorf("missing %s script", h)
		}
	}
	// Scripts never come from a downloaded catalog, even one that names the same app.
	if cat.Hook(&StoreApp{ID: "pterodactyl", Catalog: "runtipi"}, "setup") != nil {
		t.Error("a downloaded catalog must not get to run scripts")
	}
	plan, err := loadCompose(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	yml, err := plan.Build(buildInput{AppID: "pterodactyl", DataDir: "/data/apps/pterodactyl/data", Port: 20003})
	if err != nil {
		t.Fatal(err)
	}
	out := string(yml)
	for _, want := range []string{
		"20003:80",    // the panel on the port ZeroOS picked
		"28080:28080", // Wings
		// game servers are started by Wings as separate containers, so their
		// files must be at the same place inside Wings and on the computer
		"/data/apps/pterodactyl/data/data/wings/servers:/data/apps/pterodactyl/data/data/wings/servers",
		"${HOST_NAME}.local:host-gateway",
		"RECAPTCHA_ENABLED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compose file lacks %q\n%s", want, out)
		}
	}
}
