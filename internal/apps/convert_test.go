package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const jellyfin = `{"services":[{"name":"jellyfin","image":"lscr.io/linuxserver/jellyfin:1","isMain":true,"internalPort":8096,
"environment":[{"key":"TZ","value":"${TZ}"}],
"volumes":[{"hostPath":"${APP_DATA_DIR}/data/config","containerPath":"/config"},{"hostPath":"${ROOT_FOLDER_HOST}/media/data","containerPath":"/media/data"}],
"addPorts":[{"hostPort":7359,"containerPort":7359,"udp":true,"tcp":false}]}]}`

func TestConvert(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.json"), []byte(jellyfin), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := loadCompose(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := buildInput{AppID: "jellyfin", DataDir: "/ssd/apps/jellyfin/data", SharedDir: "/ssd/shared", Port: 20000,
		Folders: map[string]string{"${ROOT_FOLDER_HOST}/media/data": "/mnt/zeroos/Media/Films"}, UsesNAS: true}
	out, err := plan.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	yml := string(out)
	for _, want := range []string{
		"/ssd/apps/jellyfin/data/data/config:/config", // the app's own data stays on the SSD
		"/mnt/zeroos/Media/Films:/media/data",         // the picked NAS folder
		"20000:8096",                                  // the port ZeroOS picked
		"7359:7359/udp",
		`restart: "no"`, // apps on the NAS are started by ZeroOS, after the NAS is connected
		"zeroos.app: jellyfin",
	} {
		if !strings.Contains(yml, want) {
			t.Errorf("compose file is missing %q:\n%s", want, yml)
		}
	}
	if strings.Contains(yml, "7359:7359\n") {
		t.Errorf("a UDP-only port was also opened as TCP:\n%s", yml)
	}
	if dirs := plan.dirsToCreate(in); len(dirs) != 1 || dirs[0] != "/ssd/apps/jellyfin/data/data/config" {
		t.Errorf("dirsToCreate = %v", dirs)
	}
	in.UsesNAS = false
	out, _ = plan.Build(in)
	if !strings.Contains(string(out), "restart: unless-stopped") {
		t.Errorf("apps without NAS folders should restart by themselves")
	}
}

func TestFriendlyFolder(t *testing.T) {
	for in, want := range map[string]string{"${ROOT_FOLDER_HOST}/media/data": "Media library", "${ROOT_FOLDER_HOST}/media/torrents": "Downloads folder", "${ROOT_FOLDER_HOST}/media": "Media folder"} {
		if got := friendlyFolder(in); got != want {
			t.Errorf("friendlyFolder(%q) = %q, want %q", in, got, want)
		}
	}
}
