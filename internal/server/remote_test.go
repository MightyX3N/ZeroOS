package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsHomeHost(t *testing.T) {
	for host, want := range map[string]bool{
		"server.local": true, "Server.local:8080": true, "192.168.1.50": true, "192.168.1.50:80": true, "10.0.0.2": true,
		"localhost:8080": true, "server": true, "127.0.0.1": true,
		"hungry-fox.gl.joinmc.link": false, "some-name.ply.gg:12345": false, "8.8.8.8": false, "147.185.221.20:4321": false,
	} {
		if got := isHomeHost(host); got != want {
			t.Errorf("isHomeHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestViaTunnel(t *testing.T) {
	mk := func(remote, proto string) *http.Request {
		r := httptest.NewRequest("GET", "http://films.playit.plus/", nil)
		r.RemoteAddr = remote
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		return r
	}
	if !viaTunnel(mk("127.0.0.1:5555", "https")) || !viaTunnel(mk("[::1]:5555", "https")) {
		t.Error("the agent on this computer passing on an https request is a tunnel visit")
	}
	if viaTunnel(mk("192.168.1.50:5555", "https")) {
		t.Error("another computer can't claim to be the tunnel")
	}
	if viaTunnel(mk("127.0.0.1:5555", "")) || viaTunnel(mk("127.0.0.1:5555", "http")) {
		t.Error("a plain local request is not a tunnel visit")
	}
}
