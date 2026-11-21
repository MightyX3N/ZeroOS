package apps

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
	"github.com/MightyX3N/zeroos/internal/storage"
)

func writeRecipe(t *testing.T, dir, version, image string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"id":"notes","name":"Notes","available":true,"version":"`+version+`"}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "docker-compose.json"), []byte(`{"services":[{"name":"notes","image":"`+image+`","isMain":true,"internalPort":80}]}`), 0o644)
}

func TestSameRecipe(t *testing.T) {
	d := t.TempDir()
	a, b, c, e := filepath.Join(d, "a"), filepath.Join(d, "b"), filepath.Join(d, "c"), filepath.Join(d, "e")
	writeRecipe(t, a, "1.0", "notes:1")
	writeRecipe(t, b, "1.0", "notes:1")
	writeRecipe(t, c, "1.1", "notes:1.1")
	writeRecipe(t, e, "1.0", "notes:1-fixed") // same version number, runs differently
	if !sameRecipe(a, b) {
		t.Error("identical recipes should be the same")
	}
	if sameRecipe(a, c) || sameRecipe(a, e) {
		t.Error("a new version or a changed image is an update")
	}
	if err := copyRecipe(c, a); err != nil || !sameRecipe(a, c) || recipeVersion(a) != "1.1" {
		t.Errorf("copyRecipe: %v", err)
	}
}

// An installed app stays on its version, notices a newer one in the store,
// and refuses to update without room for the copy unless told to.
func TestUpdateDetection(t *testing.T) {
	c, err := core.Open(core.Config{DataDir: t.TempDir(), MountRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// a downloaded catalog with one app
	store := filepath.Join(c.Cfg.StoreDir(), "runtipi", "notes")
	writeRecipe(t, store, "1.0", "notes:1")
	cat := NewCatalog(c)
	m := NewManager(c, cat, storage.New(c))
	if _, err := c.DB.Exec(`INSERT INTO apps(id,store_id,catalog,name,port,state,settings,folders,created) VALUES('notes','notes','runtipi','Notes',20000,'running','{"desired":"running"}','{}',?)`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	m.Recover() // an app from before recipes existed gets its own
	a, _ := m.Get("notes")
	if a.Settings.Version != "1.0" || m.UpdateFor(a) != nil {
		t.Fatalf("version %q, update %+v", a.Settings.Version, m.UpdateFor(a))
	}
	// the catalog moves on
	writeRecipe(t, store, "2.0", "notes:2")
	_ = cat.Load()
	a, _ = m.Get("notes")
	up := m.UpdateFor(a)
	if up == nil || up.From != "1.0" || up.To != "2.0" {
		t.Fatalf("update = %+v", up)
	}
	if recipeVersion(m.recipeDir("notes")) != "1.0" {
		t.Fatal("the app's own recipe must not follow the catalog by itself")
	}
	old := freeSpace
	freeSpace = func(string) int64 { return 0 }
	defer func() { freeSpace = old }()
	_, err = m.Update("notes", false)
	var ns *ErrNoSpace
	if !errors.As(err, &ns) {
		t.Fatalf("want a no-space error, got %v", err)
	}
	// an old copy for undoing is deleted after a week
	a.Settings.Undo = &UndoPoint{Version: "0.9", At: time.Now().Add(-8 * 24 * time.Hour).Unix()}
	m.saveSettings(a)
	_ = os.MkdirAll(filepath.Join(m.undoDir("notes"), "data"), 0o755)
	m.ExpireUndo()
	a, _ = m.Get("notes")
	if a.Settings.Undo != nil || dirExists(m.undoDir("notes")) {
		t.Fatal("a week-old undo copy should be gone")
	}
}
