package storage

import (
	"context"
	"net"
	"testing"
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
