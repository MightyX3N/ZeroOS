package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Reaching ZeroOS itself from outside the home.
//
// ZeroOS also listens with encryption (HTTPS) on a second port, using a
// certificate it makes for itself. The playit tunnel points at that port, so
// passwords never cross the internet unencrypted.

// tlsPort is the encrypted port (default 8443).
func (s *Server) tlsPort() int {
	_, p, err := net.SplitHostPort(s.C.Cfg.TLSListen)
	if err != nil {
		return 8443
	}
	n, _ := strconv.Atoi(p)
	return n
}

// certificate loads ZeroOS's own certificate, making it the first time.
func (s *Server) certificate() (tls.Certificate, error) {
	certFile := filepath.Join(s.C.Cfg.DataDir, "tls-cert.pem")
	keyFile := filepath.Join(s.C.Cfg.DataDir, "tls-key.pem")
	if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "ZeroOS on " + host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host, host + ".local", "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certFile, keyFile)
}

// isHomeHost tells whether the address in the browser is one from inside the home.
func isHomeHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return host == "localhost" || strings.HasSuffix(strings.ToLower(host), ".local") || !strings.Contains(host, ".")
}

// webPort is the plain port the web UI listens on at home (default 80).
func (s *Server) webPort() int {
	_, p, err := net.SplitHostPort(s.C.Cfg.Listen)
	if err != nil {
		return 80
	}
	n, _ := strconv.Atoi(p)
	return n
}

// secureInfo describes an HTTPS tunnel found on playit for a local port.
func (s *Server) secureInfo(port int) map[string]any {
	x := s.Playit.SecureFor(port)
	if x == nil {
		return nil
	}
	return map[string]any{"address": x.URL(), "cert": x.Cert, "problem": x.Problem, "name": x.Name}
}

// viaTunnel tells whether a request came in through playit's HTTPS tunnel:
// the agent on this computer decrypts it and passes it on to the plain port.
func viaTunnel(r *http.Request) bool {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") != "https" {
		return false
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(h)
	return err == nil && ip != nil && ip.IsLoopback()
}

func (s *Server) remoteOn() bool {
	var id int64
	return s.C.DB.QueryRow(`SELECT id FROM tunnels WHERE kind='zeroos'`).Scan(&id) == nil
}

// guardRemote refuses visitors who arrive at ZeroOS's own secure address
// while "Reach ZeroOS from outside" is turned off.
func (s *Server) guardRemote(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if x := s.Playit.SecureFor(s.webPort()); x != nil && r.TLS == nil && !s.remoteOn() {
			host := r.Host
			if hh, _, err := net.SplitHostPort(host); err == nil {
				host = hh
			}
			if strings.EqualFold(host, x.Host) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("Reaching ZeroOS from outside is turned off.\n\nTurn it on at home: Settings > Sharing online > Reach ZeroOS from outside.\n"))
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) remoteInfo() map[string]any {
	var id int64
	var addr string
	on := s.C.DB.QueryRow(`SELECT id,address FROM tunnels WHERE kind='zeroos'`).Scan(&id, &addr) == nil
	out := map[string]any{"on": on, "webPort": s.webPort()}
	if on && addr != "" {
		out["address"] = "https://" + addr
	}
	if sec := s.secureInfo(s.webPort()); sec != nil {
		out["secure"] = sec
	}
	return out
}

func (s *Server) setRemote(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &in); err != nil {
		failErr(w, err)
		return
	}
	var id int64
	var pid string
	exists := s.C.DB.QueryRow(`SELECT id,playit_id FROM tunnels WHERE kind='zeroos'`).Scan(&id, &pid) == nil
	if in.On && !exists {
		if !u.TOTP {
			fail(w, 400, "Turn on two-step sign-in for your account first (Settings › Profile). It protects ZeroOS once it can be reached from the internet.")
			return
		}
		pid, err := s.Playit.CreateTunnel("ZeroOS", s.tlsPort(), "tcp")
		s.log(r, "turned on reaching ZeroOS from outside", "", "", err == nil, errText(err))
		if err != nil {
			failErr(w, err)
			return
		}
		_, _ = s.C.DB.Exec(`INSERT INTO tunnels(kind,name,local_port,proto,playit_id,created_by,created) VALUES('zeroos','ZeroOS',?,?,?,?,?)`, s.tlsPort(), "tcp", pid, u.ID, time.Now().Unix())
		go s.chaseAddresses()
	}
	if !in.On && exists {
		err := s.Playit.DeleteTunnel(pid)
		_, _ = s.C.DB.Exec(`DELETE FROM tunnels WHERE id=?`, id)
		s.log(r, "turned off reaching ZeroOS from outside", "", "", err == nil, errText(err))
	}
	ok(w, s.remoteInfo())
}
