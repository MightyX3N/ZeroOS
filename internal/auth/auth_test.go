package auth

import (
	"testing"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
)

func newAuth(t *testing.T) (*Auth, *core.Core) {
	c, err := core.Open(core.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return a, c
}

func TestSetupAndPermissions(t *testing.T) {
	a, c := newAuth(t)
	code := a.SetupCode()
	if a.CheckSetupCode("000000") || !a.CheckSetupCode(code) {
		t.Fatal("setup code check is wrong")
	}
	ownerID, err := a.CreateOwner("admin", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateOwner("evil", "password123"); err == nil {
		t.Fatal("a second owner must not be possible")
	}
	if a.CheckSetupCode(code) {
		t.Fatal("the setup code must stop working after setup")
	}
	owner, _ := a.UserByID(ownerID)
	roles, _ := a.Roles()
	var member, admin int64
	for _, r := range roles {
		switch r.Name {
		case "Member":
			member = r.ID
		case "Admin":
			admin = r.ID
		}
	}
	id, err := a.CreateUser(owner, NewUser{Username: "mads", Password: "madspass1", RoleID: member})
	if err != nil {
		t.Fatal(err)
	}
	mads, _ := a.UserByID(id)
	if mads.Can("apps.install") || mads.Can("users.manage") {
		t.Fatal("a member must not have server-wide permissions")
	}
	if _, err := c.DB.Exec(`INSERT INTO apps(id,store_id,catalog,name,created) VALUES('jellyfin','jellyfin','runtipi','Jellyfin',0)`); err != nil {
		t.Fatal(err)
	}
	if a.CanApp(mads, "jellyfin", "app.view") {
		t.Fatal("a member must not see apps that weren't shared with them")
	}
	if err := a.SetAppPerms(id, "jellyfin", []string{"app.open"}); err != nil {
		t.Fatal(err)
	}
	if !a.CanApp(mads, "jellyfin", "app.open") || !a.CanApp(mads, "jellyfin", "app.view") || a.CanApp(mads, "jellyfin", "app.remove") {
		t.Fatal("per-app permissions are wrong")
	}
	if !a.CanApp(owner, "jellyfin", "app.remove") {
		t.Fatal("the owner can do everything")
	}
	// mads can't hand out permissions he doesn't have
	if _, err := a.CreateUser(mads, NewUser{Username: "sofie", Password: "sofiepass1", RoleID: admin}); err == nil {
		t.Fatal("a member must not be able to create an admin")
	}
	if err := a.DeleteUser(ownerID); err == nil {
		t.Fatal("the owner can't be deleted")
	}
}

func TestLoginLockoutAndTOTP(t *testing.T) {
	a, _ := newAuth(t)
	id, _ := a.CreateOwner("admin", "password123")
	if _, _, err := a.Login("admin", "password123", "", "test", false); err != nil {
		t.Fatal(err)
	}
	secret, _ := NewTOTPSecret("admin")
	code, _ := totpAt(secret, time.Now())
	if err := a.EnableTOTP(id, secret, code); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Login("admin", "password123", "", "test", false); err != ErrNeedCode {
		t.Fatalf("want ErrNeedCode, got %v", err)
	}
	if _, _, err := a.Login("admin", "password123", code, "test", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, _, _ = a.Login("admin", "wrong", "", "test", false)
	}
	if _, _, err := a.Login("admin", "password123", code, "test", false); err != ErrLocked {
		t.Fatalf("want ErrLocked after 5 wrong tries, got %v", err)
	}
}
