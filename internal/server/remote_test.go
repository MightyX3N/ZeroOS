package server

import "testing"

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
