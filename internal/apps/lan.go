package apps

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Apps on the home network.
//
// The main part of every app also joins the home network as a device of its
// own: it gets an address from the router like a phone or TV does, sees the
// network's broadcasts and can make its own. That is how apps find and are
// found by the devices at home (Jellyfin's apps, casting to a TV, smart-home
// gear) without anything set up per app. The app keeps its private network
// too, so its other parts and its page on the mini PC work as before.
//
// Docker calls this a macvlan network. It needs a wired connection; on Wi-Fi,
// or when the router hands out no addresses, apps simply run without it.

const lanNet = "zeroos-lan" // the Docker network on the home network

// LANLease is an app's address on the home network.
type LANLease struct {
	IP    string `json:"ip"`
	MAC   string `json:"mac"`
	Given int64  `json:"given"`         // when the router gave it
	Until int64  `json:"until"`         // when it runs out
	ACK   string `json:"ack,omitempty"` // the router's answer, for giving the address back
}

// homeNet describes the mini PC's connection to the home network.
type homeNet struct {
	Iface   string
	Subnet  *net.IPNet
	Gateway net.IP
	HostIP  net.IP
	Why     string // why apps can't join it ("" when they can)
}

var (
	homeMu    sync.Mutex
	homeCache *homeNet
	homeAt    time.Time
)

// findHome works out which network connection leads home: the one with the
// default route, unless ZEROOS_LAN_IFACE names another.
func findHome() *homeNet {
	homeMu.Lock()
	defer homeMu.Unlock()
	if homeCache != nil && time.Since(homeAt) < time.Minute {
		return homeCache
	}
	h := &homeNet{}
	defer func() { homeCache, homeAt = h, time.Now() }()
	name := os.Getenv("ZEROOS_LAN_IFACE")
	var gw net.IP
	if routes, err := netlink.RouteList(nil, netlink.FAMILY_V4); err == nil {
		for _, r := range routes {
			if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" {
				if l, err := netlink.LinkByIndex(r.LinkIndex); err == nil && (name == "" || l.Attrs().Name == name) {
					if name == "" {
						name = l.Attrs().Name
					}
					gw = r.Gw
					break
				}
			}
		}
	}
	if name == "" {
		h.Why = "This mini PC has no network connection to the home network."
		return h
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		h.Why = "The network connection " + name + " doesn't exist."
		return h
	}
	h.Iface = name
	if _, err := os.Stat("/sys/class/net/" + name + "/wireless"); err == nil {
		h.Why = "This mini PC is on Wi-Fi. Apps get their own address on the home network only over a network cable."
		return h
	}
	if _, err := os.Stat("/sys/class/net/" + name + "/phy80211"); err == nil {
		h.Why = "This mini PC is on Wi-Fi. Apps get their own address on the home network only over a network cable."
		return h
	}
	addrs, _ := netlink.AddrList(link, netlink.FAMILY_V4)
	for _, a := range addrs {
		if a.IP.IsGlobalUnicast() {
			h.HostIP, h.Subnet = a.IP, &net.IPNet{IP: a.IP.Mask(a.Mask), Mask: a.Mask}
			break
		}
	}
	if h.Subnet == nil {
		h.Why = "The network connection " + name + " has no address."
		return h
	}
	if gw == nil || !h.Subnet.Contains(gw) { // ZEROOS_LAN_IFACE without its own default route
		gw = make(net.IP, 4)
		copy(gw, h.Subnet.IP.To4())
		gw[3]++
	}
	h.Gateway = gw
	return h
}

// LANOn tells whether apps get their own address on the home network (Settings › System).
func (m *Manager) LANOn() bool { return m.c.Setting("lan.apps") != "off" }

// LANStatus says whether apps can be on the home network, and if not, why.
func (m *Manager) LANStatus() (on bool, iface, why string) {
	h := findHome()
	if !m.LANOn() {
		return false, h.Iface, ""
	}
	return h.Why == "", h.Iface, h.Why
}

// appMAC is the app's network card address: made up, but the same every time,
// so the router keeps giving the app the same address.
func appMAC(id string) net.HardwareAddr {
	machine, _ := os.ReadFile("/etc/machine-id")
	sum := sha256.Sum256(append(machine, []byte("zeroos-lan:"+id)...))
	mac := net.HardwareAddr(sum[:6])
	mac[0] = (mac[0] | 0x02) &^ 0x01 // locally administered, not multicast
	return mac
}

// ensureLANNetwork makes sure the Docker network on the home network exists
// and fits the current connection.
func (m *Manager) ensureLANNetwork(h *homeNet) error {
	m.lanNetMu.Lock()
	defer m.lanNetMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", lanNet, "-f",
		`{{index .Options "parent"}} {{range .IPAM.Config}}{{.Subnet}} {{.Gateway}}{{end}}`).Output()
	want := fmt.Sprintf("%s %s %s", h.Iface, h.Subnet.String(), h.Gateway.String())
	if err == nil && strings.TrimSpace(string(out)) == want {
		return nil
	}
	if err == nil { // the home network changed (another router, another cable): start over
		log.Printf("lan: home network changed from %q to %q", strings.TrimSpace(string(out)), want)
		ids, _ := exec.CommandContext(ctx, "docker", "network", "inspect", lanNet, "-f", "{{range $k, $v := .Containers}}{{$k}} {{end}}").Output()
		for _, c := range strings.Fields(string(ids)) {
			_ = exec.CommandContext(ctx, "docker", "network", "disconnect", "-f", lanNet, c).Run()
		}
		_ = exec.CommandContext(ctx, "docker", "network", "rm", lanNet).Run()
	}
	b, err := exec.CommandContext(ctx, "docker", "network", "create", "-d", "macvlan", "--subnet", h.Subnet.String(),
		"--gateway", h.Gateway.String(), "-o", "parent="+h.Iface, "--label", "zeroos=lan", lanNet).CombinedOutput()
	if err != nil {
		if strings.Contains(string(b), "overlaps") { // another Docker network already uses the home network's addresses
			return fmt.Errorf("another Docker network on this computer already uses the home network's addresses (%s): %s", h.Subnet, strings.TrimSpace(string(b)))
		}
		return fmt.Errorf("docker network create: %s", strings.TrimSpace(string(b)))
	}
	return nil
}

// dhcpAsk asks the router for an address for the app, from a short-lived
// network card on this computer with the app's own card address.
func dhcpAsk(h *homeNet, id string, mac net.HardwareAddr, want net.IP) (*nclient4.Lease, error) {
	parent, err := netlink.LinkByName(h.Iface)
	if err != nil {
		return nil, err
	}
	name := "zl" + fmt.Sprintf("%x", sha256.Sum256([]byte(id)))[:10]
	_ = netlink.LinkDel(&netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: name}})
	link := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: name, ParentIndex: parent.Attrs().Index, HardwareAddr: mac}, Mode: netlink.MACVLAN_MODE_BRIDGE}
	if err := netlink.LinkAdd(link); err != nil {
		return nil, fmt.Errorf("making a network card for the app: %w", err)
	}
	defer netlink.LinkDel(link)
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, err
	}
	time.Sleep(300 * time.Millisecond)
	return dhcpRequest(name, id, mac, want)
}

func dhcpRequest(iface, id string, mac net.HardwareAddr, want net.IP) (*nclient4.Lease, error) {
	c, err := nclient4.New(iface, nclient4.WithHWAddr(mac), nclient4.WithTimeout(3*time.Second), nclient4.WithRetry(2))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return dhcpDo(c, id, want)
}

func dhcpDo(c *nclient4.Client, id string, want net.IP) (*nclient4.Lease, error) {
	mods := []dhcpv4.Modifier{dhcpv4.WithOption(dhcpv4.OptHostName(id))}
	if want != nil {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(want)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	return c.Request(ctx, mods...)
}

// inContainerNet runs fn inside a running container's network.
func inContainerNet(container string, fn func() error) error {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Pid}}", container).Output()
	if err != nil {
		return err
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if pid <= 0 {
		return errors.New("not running")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	home, err := netns.Get()
	if err != nil {
		return err
	}
	defer home.Close()
	target, err := netns.GetFromPid(pid)
	if err != nil {
		return err
	}
	defer target.Close()
	if err := netns.Set(target); err != nil {
		return err
	}
	defer netns.Set(home)
	return fn()
}

func leaseOf(l *nclient4.Lease, mac net.HardwareAddr) *LANLease {
	secs := 3600
	if d := l.ACK.IPAddressLeaseTime(time.Hour); d > 0 {
		secs = int(d.Seconds())
	}
	now := time.Now().Unix()
	return &LANLease{IP: l.ACK.YourIPAddr.String(), MAC: mac.String(), Given: now, Until: now + int64(secs),
		ACK: base64.StdEncoding.EncodeToString(l.ACK.ToBytes())}
}

// askLease asks the router for the app's address, without changing anything:
// from inside the app when it runs with its card, else from a short-lived card.
func (m *Manager) askLease(a *App, h *homeNet) (*LANLease, error) {
	mac := appMAC(a.ID)
	var want net.IP
	if old := a.Settings.LAN; old != nil {
		want = net.ParseIP(old.IP)
	} else if a.Settings.LastLAN != "" && h.Subnet.Contains(net.ParseIP(a.Settings.LastLAN)) {
		want = net.ParseIP(a.Settings.LastLAN)
	}
	var l *nclient4.Lease
	var err error
	if c := m.lanContainer(a.ID); c != "" {
		err = inContainerNet(c, func() error {
			iface, e := ifaceWithMAC(mac)
			if e != nil {
				return e
			}
			l, e = dhcpRequest(iface, a.ID, mac, want)
			return e
		})
	} else {
		l, err = dhcpAsk(h, a.ID, mac, want)
	}
	if err != nil {
		m.mu.Lock() // ask less and less often while the router doesn't answer
		wait := m.lanWait[a.ID] * 2
		if wait < 3*time.Minute {
			wait = 3 * time.Minute
		}
		if wait > time.Hour {
			wait = time.Hour
		}
		m.lanWait[a.ID], m.lanNext[a.ID] = wait, time.Now().Add(wait)
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Lock()
	delete(m.lanWait, a.ID)
	delete(m.lanNext, a.ID)
	m.mu.Unlock()
	return leaseOf(l, mac), nil
}

// lanFor gives the app an address on the home network before it starts. The
// caller holds the app's lock. Without one the app runs as before, on its
// private network only, and ZeroOS keeps trying in the background.
func (m *Manager) lanFor(a *App, wanted bool) *LANLease {
	if on, _, _ := m.LANStatus(); !on || !wanted {
		m.dropLAN(a, false)
		return nil
	}
	h := findHome()
	if err := m.ensureLANNetwork(h); err != nil {
		log.Printf("lan: %v", err)
		return nil
	}
	old := a.Settings.LAN
	if old != nil && old.MAC == appMAC(a.ID).String() && h.Subnet.Contains(net.ParseIP(old.IP)) && !old.due() {
		return old // still good
	}
	m.mu.Lock()
	next := m.lanNext[a.ID]
	m.mu.Unlock()
	if old == nil && time.Now().Before(next) {
		return nil // the router didn't answer a moment ago: don't hold up the start, try again later
	}
	l, err := m.askLease(a, h)
	if err != nil {
		log.Printf("lan: asking the router for %s's address: %v", a.ID, err)
		if old != nil && time.Now().Unix() < old.Until {
			return old
		}
		if old != nil {
			a.Settings.LAN = nil
			m.saveSettings(a)
		}
		return nil
	}
	m.setLease(a, l)
	return l
}

func (m *Manager) setLease(a *App, l *LANLease) {
	old := a.Settings.LAN
	a.Settings.LAN, a.Settings.LastLAN = l, l.IP
	m.saveSettings(a)
	if old == nil || old.IP != l.IP {
		m.c.Log("system", "got an address on the home network for", a.Name+" ("+l.IP+")", a.ID, true, "")
	}
}

func ifaceWithMAC(mac net.HardwareAddr) (string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return "", err
	}
	for _, l := range links {
		if l.Attrs().HardwareAddr.String() == mac.String() {
			return l.Attrs().Name, nil
		}
	}
	return "", errors.New("the app's network card isn't there")
}

// dropLAN gives the app's address back to the router.
func (m *Manager) dropLAN(a *App, tell bool) {
	l := a.Settings.LAN
	if l == nil {
		return
	}
	a.Settings.LAN = nil
	m.saveSettings(a)
	raw, err := base64.StdEncoding.DecodeString(l.ACK)
	if err != nil {
		return
	}
	ack, err := dhcpv4.FromBytes(raw)
	if err != nil {
		return
	}
	// Best effort: the router takes it back by itself when the lease runs out.
	send := func() error { return sendRelease(ack, net.ParseIP(l.IP)) }
	var err2 error
	if c := m.lanContainer(a.ID); c != "" { // still running with it: from inside, where its card is
		err2 = inContainerNet(c, send)
	} else {
		err2 = send()
	}
	if err2 != nil {
		log.Printf("lan: giving back %s's address: %v", a.ID, err2)
	}
	if tell {
		m.c.Log("system", "gave back the home network address of", a.Name, a.ID, true, "")
	}
}

// lanContainer is the running container of the app's main part that is on the home network.
func (m *Manager) lanContainer(id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "label=zeroos.app="+id, "--filter", "network="+lanNet).Output()
	if f := strings.Fields(string(out)); len(f) > 0 {
		return f[0]
	}
	return ""
}

// fixHostRoute keeps an app on the home network reachable the old ways too.
//
// Its answers to visitors who came through the mini PC's own address (the
// page on ZeroOS's port) have to go back the way they came, through the
// private network, not out of its own card on the home network: the visitor
// would throw away an answer from an address it never asked. And the app has
// to reach the mini PC itself (ZeroOS, other apps' pages) the private way,
// because a macvlan card can't talk to the computer it sits on.
func fixHostRoute(container string, h *homeNet) error {
	out, err := exec.Command("docker", "inspect", "-f", `{{range $k, $v := .NetworkSettings.Networks}}{{if ne $k "`+lanNet+`"}}{{$v.IPAddress}}/{{$v.IPPrefixLen}} {{$v.Gateway}} {{end}}{{end}}`, container).Output()
	if err != nil {
		return err
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return nil
	}
	ip, private, err := net.ParseCIDR(f[0])
	if err != nil {
		return err
	}
	gw := net.ParseIP(f[1])
	return inContainerNet(container, func() error {
		routes, err := netlink.RouteGet(gw)
		if err != nil || len(routes) == 0 {
			return fmt.Errorf("no way to the private network: %v", err)
		}
		link := routes[0].LinkIndex
		if err := netlink.RouteReplace(&netlink.Route{Dst: &net.IPNet{IP: h.HostIP, Mask: net.CIDRMask(32, 32)}, Gw: gw, LinkIndex: link}); err != nil {
			return err
		}
		const table = 100
		if err := netlink.RouteReplace(&netlink.Route{Dst: private, LinkIndex: link, Scope: netlink.SCOPE_LINK, Table: table}); err != nil {
			return err
		}
		if err := netlink.RouteReplace(&netlink.Route{Gw: gw, LinkIndex: link, Table: table}); err != nil {
			return err
		}
		rules, _ := netlink.RuleList(netlink.FAMILY_V4)
		for _, r := range rules {
			if r.Table == table && r.Src != nil && r.Src.IP.Equal(ip) {
				return nil
			}
		}
		rule := netlink.NewRule()
		rule.Src = &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}
		rule.Table = table
		rule.Priority = 100
		return netlink.RuleAdd(rule)
	})
}

// KeepLAN keeps the apps' home network addresses alive: it renews them in
// time and fixes the way to the mini PC each time an app starts. It never returns.
func (m *Manager) KeepLAN() {
	starts := make(chan string, 64)
	go func() {
		for {
			cmd := exec.Command("docker", "events", "--filter", "type=container", "--filter", "event=start", "--filter", "label=zeroos.app", "--format", "{{.Actor.ID}}")
			out, err := cmd.StdoutPipe()
			if err == nil && cmd.Start() == nil {
				buf := make([]byte, 4096)
				var line []byte
				for {
					n, err := out.Read(buf)
					for _, b := range buf[:n] {
						if b == '\n' {
							starts <- string(line)
							line = line[:0]
						} else {
							line = append(line, b)
						}
					}
					if err != nil {
						break
					}
				}
				_ = cmd.Wait()
			}
			time.Sleep(5 * time.Second)
		}
	}()
	fixAll := func() {
		h := findHome()
		if h.Why != "" {
			return
		}
		out, _ := exec.Command("docker", "ps", "-q", "--filter", "network="+lanNet).Output()
		for _, c := range strings.Fields(string(out)) {
			_ = fixHostRoute(c, h)
		}
	}
	fixAll()
	tick := time.NewTicker(time.Minute)
	for {
		select {
		case c := <-starts:
			time.Sleep(time.Second)
			if h := findHome(); h.Why == "" {
				if err := fixHostRoute(c, h); err != nil && !strings.Contains(err.Error(), "not running") {
					log.Printf("lan: %v", err)
				}
			}
		case <-tick.C:
			m.renewLeases()
		}
	}
}

// renewLeases asks the router again for addresses that are halfway to
// running out, and tries again for apps that couldn't get one before (the
// router was off, say). The asking happens without holding up the app; only an
// app whose address changes (got one, lost it, or another one) is started again.
func (m *Manager) renewLeases() {
	on, _, _ := m.LANStatus()
	h := findHome()
	if !on || h.Why != "" {
		return
	}
	list, err := m.List()
	if err != nil {
		return
	}
	for _, a := range list {
		l := a.Settings.LAN
		var try bool
		if l != nil { // whatever ZeroOS is doing with the app, a running app keeps its address
			try = l.due() && m.lanContainer(a.ID) != ""
		} else if a.State == "running" {
			m.mu.Lock()
			next := m.lanNext[a.ID]
			m.mu.Unlock()
			try = time.Now().After(next) && m.wantsLAN(a)
		}
		if !try {
			continue
		}
		got, err := m.askLease(a, h)
		if err != nil && (l == nil || time.Now().Unix() < l.Until) {
			continue // keeps what it has; asks again later
		}
		locked := false
		for end := time.Now().Add(time.Minute); !locked && time.Now().Before(end); time.Sleep(time.Second) {
			locked = m.lock(a.ID)
		}
		if !locked {
			continue
		}
		cur, gerr := m.Get(a.ID)
		if gerr == nil {
			before := ""
			if cur.Settings.LAN != nil {
				before = cur.Settings.LAN.IP
			}
			if got != nil {
				m.setLease(cur, got)
			} else {
				cur.Settings.LAN = nil
				m.saveSettings(cur)
				m.c.Log("system", "lost its address on the home network (the router didn't answer)", cur.Name, cur.ID, false, "")
			}
			after := ""
			if cur.Settings.LAN != nil {
				after = cur.Settings.LAN.IP
			}
			if after != before && cur.State == "running" {
				if m.writeCompose(cur.ID, false) == nil {
					_ = m.compose(cur.ID, nil, "up", "-d")
				}
			}
		}
		m.unlock(a.ID)
	}
}

// wantsLAN tells whether the app has a part that would join the home network.
func (m *Manager) wantsLAN(a *App) bool {
	plan, err := loadCompose(m.recipeDir(a.ID))
	if err != nil {
		return false
	}
	in := buildInput{AppID: a.ID}
	if cfg, ok := readCustom(m.recipeDir(a.ID)); ok {
		in.Custom = &cfg.Custom
	}
	return plan.lanService(in) != ""
}

// due: past the middle of the lease, when a device asks the router again.
func (l *LANLease) due() bool {
	now := time.Now().Unix()
	return now >= l.Given+(l.Until-l.Given)/2
}

// ApplyLAN starts every running app again on (or off) the home network, after the setting changed.
func (m *Manager) ApplyLAN() {
	list, err := m.List()
	if err != nil {
		return
	}
	for _, a := range list {
		if a.State != "running" {
			continue
		}
		got := false // an app that is busy (being updated, backed up) gets its turn when that is done
		for end := time.Now().Add(10 * time.Minute); time.Now().Before(end); time.Sleep(2 * time.Second) {
			if got = m.lock(a.ID); got {
				break
			}
		}
		if !got {
			log.Printf("lan: %s stayed busy; it joins or leaves the home network when it next starts", a.ID)
			continue
		}
		if b, err := m.Get(a.ID); err == nil && b.State == "running" && m.writeCompose(a.ID, false) == nil {
			_ = m.compose(a.ID, nil, "up", "-d", "--remove-orphans")
		}
		m.unlock(a.ID)
	}
}

// sendRelease tells the router the app no longer needs its address. It is
// sent from that address, which most routers insist on, even when the app's
// own network card is already gone.
func sendRelease(ack *dhcpv4.DHCPv4, ip net.IP) error {
	req, err := dhcpv4.NewReleaseFromACK(ack)
	if err != nil {
		return err
	}
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		err := rc.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1)
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TRANSPARENT, 1) // send from an address this computer doesn't have
		})
		if err != nil {
			return err
		}
		return serr
	}}
	conn, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort(ip.String(), "68"))
	if err != nil {
		if conn, err = lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort(ip.String(), "0")); err != nil {
			return err
		}
	}
	defer conn.Close()
	_, err = conn.WriteTo(req.ToBytes(), &net.UDPAddr{IP: ack.ServerIdentifier(), Port: 67})
	return err
}
