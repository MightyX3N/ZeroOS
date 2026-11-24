package apps

import (
	"context"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Finding apps on the home network.
//
// Some apps are found by their own phone and TV apps with a shout to the whole
// network ("is there a Jellyfin here?"). Such a shout reaches this computer but
// not the app, which sits on a private network inside Docker. ZeroOS listens in
// its place, hands each question to the app and passes the answers back. Which
// apps are asked on which port is in overrides.json ("discovery").

type relay struct {
	conn net.PacketConn
	busy chan struct{}
}

// KeepDiscovery opens and closes the listeners as apps come and go. It never returns.
func (m *Manager) KeepDiscovery() {
	relays := map[int]*relay{}
	failed := map[int]bool{}
	for ; ; time.Sleep(20 * time.Second) {
		want := m.discoveryPorts()
		for port, r := range relays {
			if len(want[port]) == 0 {
				r.conn.Close()
				delete(relays, port)
			}
		}
		for port := range want {
			if relays[port] != nil {
				continue
			}
			conn, err := net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", itoa(port)))
			if err != nil {
				if !failed[port] {
					log.Printf("discovery: can't listen on port %d: %v", port, err)
					failed[port] = true
				}
				continue
			}
			delete(failed, port)
			r := &relay{conn: conn, busy: make(chan struct{}, 16)}
			relays[port] = r
			go m.serveRelay(r, port)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// discoveryPorts lists, per port, the running apps that want to be found on it.
func (m *Manager) discoveryPorts() map[int][]string {
	out := map[int][]string{}
	list, err := m.List()
	if err != nil {
		return out
	}
	for _, a := range list {
		sa, ok := m.cat.Get(a.StoreID)
		if !ok || a.State != "running" {
			continue
		}
		for _, p := range sa.discovery {
			out[p] = append(out[p], a.ID)
		}
	}
	return out
}

func (m *Manager) serveRelay(r *relay, port int) {
	buf := make([]byte, 2048)
	for {
		n, from, err := r.conn.ReadFrom(buf)
		if err != nil {
			return // closed
		}
		question := append([]byte(nil), buf[:n]...)
		select {
		case r.busy <- struct{}{}:
		default:
			continue // too many at once; the asker tries again
		}
		go func() {
			defer func() { <-r.busy }()
			var wg sync.WaitGroup
			for _, id := range m.discoveryPorts()[port] {
				for _, ip := range containerIPs(id) {
					if src, ok := from.(*net.UDPAddr); ok && src.IP.String() == ip {
						continue // the app itself
					}
					wg.Add(1)
					go func(ip string) {
						defer wg.Done()
						c, err := net.DialTimeout("udp4", net.JoinHostPort(ip, itoa(port)), time.Second)
						if err != nil {
							return
						}
						defer c.Close()
						_ = c.SetDeadline(time.Now().Add(1500 * time.Millisecond))
						if _, err := c.Write(question); err != nil {
							return
						}
						answer := make([]byte, 4096)
						for {
							n, err := c.Read(answer)
							if err != nil {
								return
							}
							_, _ = r.conn.WriteTo(answer[:n], from)
						}
					}(ip)
				}
			}
			wg.Wait()
		}()
	}
}

var (
	ipCacheMu sync.Mutex
	ipCache   = map[string]ipEntry{}
)

type ipEntry struct {
	ips []string
	at  time.Time
}

// containerIPs are the addresses of an app's containers on their private networks.
func containerIPs(id string) []string {
	ipCacheMu.Lock()
	e, ok := ipCache[id]
	ipCacheMu.Unlock()
	if ok && time.Since(e.at) < 15*time.Second {
		return e.ips
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ids, _ := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "label=zeroos.app="+id).Output()
	var ips []string
	if f := strings.Fields(string(ids)); len(f) > 0 {
		args := append([]string{"inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}"}, f...)
		out, _ := exec.CommandContext(ctx, "docker", args...).Output()
		ips = strings.Fields(string(out))
	}
	ipCacheMu.Lock()
	ipCache[id] = ipEntry{ips, time.Now()}
	ipCacheMu.Unlock()
	return ips
}
