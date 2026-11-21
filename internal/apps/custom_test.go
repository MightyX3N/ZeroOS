package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pastedCompose = `version: "3.8"
services:
  web:
    image: nginx:1.27
    container_name: mysite
    ports:
      - "8080:80"
      - "8443:443"
    labels:
      - "com.example.note=keep me"
    environment:
      - SITE_NAME=${SITE_NAME}
      - MODE=${MODE:-prod}
    volumes:
      - ./conf:/etc/nginx/conf.d
      - webdata:/usr/share/nginx/html
      - /mnt/photos:/photos:ro
      - /var/run/docker.sock:/var/run/docker.sock
    networks: [inner]
  db:
    image: postgres:16
    volumes:
      - dbdata:/var/lib/postgresql/data
    networks: [inner]
volumes:
  webdata:
  dbdata:
networks:
  inner:
`

func TestCheckCompose(t *testing.T) {
	c, _, err := CheckCompose(pastedCompose)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Services) != 2 || len(c.Ports) != 2 || c.Ports[0].Container != 80 || c.Ports[0].Published != "8080" {
		t.Errorf("services %v ports %+v", c.Services, c.Ports)
	}
	if len(c.Folders) != 1 || c.Folders[0].Key != "/mnt/photos" || c.Folders[0].Container != "/photos" {
		t.Errorf("only the folder for files is asked for: %+v", c.Folders)
	}
	if len(c.Vars) != 1 || c.Vars[0] != "SITE_NAME" {
		t.Errorf("only variables without a default are asked for: %v", c.Vars)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "Docker") {
		t.Errorf("warnings: %v", c.Warnings)
	}
	if err := checkPage(c, CustomPage{Kind: "port", Service: "web", Port: 80}); err != nil {
		t.Error(err)
	}
	if err := checkPage(c, CustomPage{Kind: "port", Service: "web", Port: 81}); err == nil {
		t.Error("a port that isn't in the file can't be the page")
	}
	for name, text := range map[string]string{
		"empty":    "  ",
		"not yaml": "services:\n  web:\n image: x",
		"nothing":  "volumes:\n  x:\n",
		"build":    "services:\n  web:\n    build: .\n",
		"env_file": "services:\n  web:\n    image: x\n    env_file: .env\n",
		"no image": "services:\n  web:\n    ports: [\"80:80\"]\n",
	} {
		if _, _, err := CheckCompose(text); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}

func TestBuildCustom(t *testing.T) {
	dir := t.TempDir()
	req := CustomRequest{Name: "My site", Compose: pastedCompose, Page: CustomPage{Kind: "port", Service: "web", Port: 80}}
	if err := writeCustomRecipe(dir, "my-site", req); err != nil {
		t.Fatal(err)
	}
	cfg, ok := readCustom(dir)
	if !ok || cfg.NoGUI {
		t.Fatal("recipe not read back")
	}
	plan, err := loadCompose(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := buildInput{AppID: "my-site", DataDir: "/ssd/apps/my-site/data", Port: 20007, Custom: &cfg.Custom,
		Folders: map[string]string{"/mnt/photos": "/mnt/zeroos/Media/Photos"}}
	yml, err := plan.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	out := string(yml)
	for _, want := range []string{
		"20007:80", // the page moved to ZeroOS's port
		"8443:443", // other ports as written
		"/ssd/apps/my-site/data/files/conf:/etc/nginx/conf.d", // relative folders land in the app's folder
		"webdata:/usr/share/nginx/html",                       // named volumes stay volumes
		"device: /ssd/apps/my-site/data/volumes/webdata",      // but their files live in the app's folder
		"device: /ssd/apps/my-site/data/volumes/dbdata",
		"/mnt/zeroos/Media/Photos:/photos:ro", // the picked folder replaces the written one
		"/var/run/docker.sock:/var/run/docker.sock",
		"com.example.note=keep me", "zeroos.app=my-site",
		"container_name: mysite", "inner",
		"restart: unless-stopped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compose file lacks %q", want)
		}
	}
	for _, not := range []string{"8080:80", "/mnt/photos:", "version:"} {
		if strings.Contains(out, not) {
			t.Errorf("compose file still has %q", not)
		}
	}
	dirs := strings.Join(plan.dirsToCreate(in), " ")
	for _, want := range []string{"/data/volumes/webdata", "/data/volumes/dbdata", "/data/files/conf"} {
		if !strings.Contains(dirs, want) {
			t.Errorf("folder %s isn't made beforehand: %s", want, dirs)
		}
	}
	// a page on a fixed port: nothing is moved
	req.Page = CustomPage{Kind: "fixed", Port: 8080}
	_ = writeCustomRecipe(dir, "my-site", req)
	cfg, _ = readCustom(dir)
	in.Custom, in.Port = &cfg.Custom, 8080
	yml, _ = plan.Build(in)
	if !strings.Contains(string(yml), "8080:80") {
		t.Error("with a fixed page port the file's ports stay as written")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		t.Error(err)
	}
}
