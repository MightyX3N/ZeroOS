package apps

import "testing"

func TestMergeVerified(t *testing.T) {
	prev := VerifiedFile{Apps: map[string]VerifiedEntry{"old": {Version: "1"}, "broke": {Version: "1"}, "offline": {Version: "1"}}}
	got := MergeVerified(prev, []VerifyResult{
		{ID: "new", Version: "2", OK: true}, {ID: "broke", Version: "2"}, {ID: "never"}, {ID: "offline", Skipped: true}, {ID: "newskip", Skipped: true},
	}, 99)
	for id, want := range map[string]bool{"old": true, "new": true, "broke": false, "never": false, "offline": true, "newskip": false} {
		if _, ok := got.Apps[id]; ok != want {
			t.Errorf("%s: in list = %v, want %v", id, ok, want)
		}
	}
	if got.Apps["new"].Version != "2" || got.Apps["new"].At != 99 || got.At != 99 {
		t.Errorf("%+v", got)
	}
}

func TestSampleAnswer(t *testing.T) {
	six := 6
	for _, c := range []struct {
		f    FormField
		want string
	}{
		{FormField{Type: "text", Default: "abc"}, "abc"},
		{FormField{Type: "text"}, ""},
		{FormField{Type: "email", Required: true}, "test@example.com"},
		{FormField{Type: "text", Required: true, Max: &six}, "zeroos"},
		{FormField{Type: "random", Required: true}, ""},
	} {
		if got := sampleAnswer(c.f); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.f, got, c.want)
		}
	}
	if got := sampleAnswer(FormField{Type: "password", Required: true}); len(got) < 8 {
		t.Errorf("password too short: %q", got)
	}
}

func TestMainOnHost(t *testing.T) {
	p := &composePlan{services: []rtService{{Name: "db"}, {Name: "plex", IsMain: true, InternalPort: "32400", NetworkMode: "host"}}}
	if host, port := p.mainOnHost(); !host || port != 32400 {
		t.Errorf("got %v %d", host, port)
	}
	p = &composePlan{services: []rtService{{Name: "web", IsMain: true, InternalPort: "80"}}}
	if host, _ := p.mainOnHost(); host {
		t.Error("a normal app is not on the host's network")
	}
}
