package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const casaSample = `name: films
services:
  films:
    image: lscr.io/linuxserver/films:1.2.3
    devices:
      - /dev/does-not-exist:/dev/does-not-exist
    environment:
      PUID: $PUID
      TZ: $TZ
    healthcheck:
      test: ["CMD-SHELL", "check $$HOME"]
    ports:
      - target: 8096
        published: "8097"
        protocol: tcp
      - target: 1900
        published: "1901"
        protocol: udp
    volumes:
      - type: bind
        source: /DATA/AppData/$AppID/config
        target: /config
      - type: bind
        source: /DATA/Media
        target: /media
      - /opt/vc/lib:/opt/vc/lib
    x-casaos:
      volumes:
        - container: /media
          description:
            en_us: Your films
  db:
    image: postgres:16
    volumes:
      - /DATA/AppData/films/pg:/var/lib/postgresql/data
x-casaos:
  architectures: [amd64, arm64]
  main: films
  category: Media
  port_map: "8097"
  index: /web/
  tagline:
    en_us: Watch your films
  description:
    en_us: A place for films.
  tips:
    before_install:
      en_us: The first account you make is the admin.
  title:
    en_us: Films
  version: "1.2.3"
`

func TestConvertCasa(t *testing.T) {
	cfg, yml, err := convertCasa([]byte(casaSample))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID != "films" || cfg.Name != "Films" || cfg.ShortDesc != "Watch your films" || cfg.Version != "1.2.3" || cfg.URLSuffix != "/web/" ||
		cfg.Categories[0] != "media" || !strings.Contains(cfg.Description, "Good to know") || cfg.NoGUI {
		t.Fatalf("config: %+v", cfg)
	}
	if p := cfg.Custom.Page; p.Kind != "port" || p.Service != "films" || p.Port != 8096 || !cfg.Custom.Casa {
		t.Fatalf("page: %+v", cfg.Custom)
	}
	if len(cfg.Folders) != 1 || cfg.Folders["/DATA/Media"].Label != "Media folder" || !strings.HasPrefix(cfg.Folders["/DATA/Media"].Hint, "Your films.") {
		t.Fatalf("folders: %+v", cfg.Folders)
	}
	if len(cfg.FormFields) != 0 { // $PUID, $TZ are given; $$HOME is not a variable
		t.Fatalf("form: %+v", cfg.FormFields)
	}
	if len(cfg.Images) != 1 || cfg.Images[0] != "linuxserver/films" {
		t.Fatalf("images: %v", cfg.Images)
	}
	s := string(yml)
	for _, want := range []string{"source: ./config", "./pg:/var/lib/postgresql/data", "source: /DATA/Media"} {
		if !strings.Contains(s, want) {
			t.Errorf("compose lacks %q:\n%s", want, s)
		}
	}
	for _, not := range []string{"x-casaos", "/opt/vc", "AppID", "name: films"} {
		if strings.Contains(s, not) {
			t.Errorf("compose still has %q", not)
		}
	}
	// built like an app of your own: page on ZeroOS's port, missing devices dropped
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), yml, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := loadCompose(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := plan.Build(buildInput{AppID: "films", DataDir: "/data", Port: 20001, Folders: map[string]string{"/DATA/Media": "/mnt/nas/films"}, Custom: &cfg.Custom})
	if err != nil {
		t.Fatal(err)
	}
	b := string(out)
	for _, want := range []string{"20001:8096", "/mnt/nas/films", "/data/files/config", "/data/files/pg", "published: \"1901\""} {
		if !strings.Contains(b, want) {
			t.Errorf("built file lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "does-not-exist") {
		t.Errorf("a device this computer lacks was kept:\n%s", b)
	}
}

func TestConvertCasaSkips(t *testing.T) {
	for name, text := range map[string]string{
		"other architecture": strings.Replace(casaSample, "[amd64, arm64]", "[riscv64]", 1),
		"nvidia":             strings.Replace(casaSample, "    image: lscr.io/linuxserver/films:1.2.3", "    image: x/y:1\n    runtime: nvidia", 1),
	} {
		if _, _, err := convertCasa([]byte(text)); err == nil {
			t.Errorf("%s: should be left out", name)
		}
	}
}

func TestImageRepo(t *testing.T) {
	for in, want := range map[string]string{"lscr.io/linuxserver/jellyfin:10.9": "linuxserver/jellyfin", "postgres:16": "postgres", "docker.io/library/redis": "redis",
		"ghcr.io/a/b@sha256:abc": "a/b", "localhost:5000/x/y:1": "x/y"} {
		if got := imageRepo(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}
