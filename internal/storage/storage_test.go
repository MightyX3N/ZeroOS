package storage

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MightyX3N/zeroos/internal/core"
)

func TestLooksLikeNAS(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	old := nasPorts
	defer func() { nasPorts = old }()

	nasPorts = []string{"1", port} // something answers on one of the management ports
	if !looksLikeNAS(context.Background(), "127.0.0.1") {
		t.Error("a computer with a management page should count as a NAS")
	}
	nasPorts = []string{"1", "2"} // nothing does: a PC that only shares folders
	if looksLikeNAS(context.Background(), "127.0.0.1") {
		t.Error("a computer without one should not")
	}
}

// A busy NAS answers late without being gone: it must stay "connected", or
// ZeroOS would stop apps exactly when they work hardest. And showing a page
// must never wait for the NAS.
func TestShareHealth(t *testing.T) {
	c, err := core.Open(core.Config{DataDir: t.TempDir(), MountRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	st := New(c)
	oldStat, oldMounted, oldWait := statfs, isMounted, probeWait
	defer func() { statfs, isMounted, probeWait = oldStat, oldMounted, oldWait }()
	probeWait = 40 * time.Millisecond
	isMounted = func(string) bool { return true }
	var calls atomic.Int32
	mode := atomic.Value{} // "ok", "slow", "gone"
	mode.Store("ok")
	statfs = func(string) (Usage, error) {
		calls.Add(1)
		switch mode.Load() {
		case "slow":
			time.Sleep(120 * time.Millisecond)
			return Usage{Total: 100, Free: 50, Used: 50}, nil
		case "gone":
			return Usage{}, errors.New("host is down")
		}
		return Usage{Total: 100, Free: 50, Used: 50}, nil
	}
	const mp = "/mnt/zeroos/Media"

	st.probe(mp)
	if !st.Healthy(mp) {
		t.Fatal("a share that answers is connected")
	}
	// showing pages asks memory, not the NAS
	before := calls.Load()
	for i := 0; i < 50; i++ {
		st.Healthy(mp)
	}
	if calls.Load() != before {
		t.Fatalf("reading the state asked the NAS %d more times", calls.Load()-before)
	}

	mode.Store("slow")
	st.probe(mp)
	time.Sleep(150 * time.Millisecond)
	st.probe(mp)
	if !st.Healthy(mp) {
		t.Fatal("two late answers: the NAS is busy, not gone")
	}
	time.Sleep(150 * time.Millisecond)
	st.probe(mp)
	if st.Healthy(mp) {
		t.Fatal("three late answers in a row: treat it as gone")
	}
	time.Sleep(150 * time.Millisecond)

	mode.Store("ok")
	st.probe(mp)
	if !st.Healthy(mp) || st.known(mp).usage == nil {
		t.Fatal("it answers again: connected")
	}
	mode.Store("gone")
	st.probe(mp)
	if st.Healthy(mp) {
		t.Fatal("a clear no counts at once")
	}
	isMounted = func(string) bool { return false }
	mode.Store("ok")
	st.probe(mp)
	if st.Healthy(mp) {
		t.Fatal("not mounted is not connected")
	}
}
