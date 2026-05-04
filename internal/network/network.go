// Package network provides per-VM L2/L3 plumbing for hyperfleet: a single host
// bridge that all microVM tap devices attach to, IP allocation out of a
// configurable subnet, and SNAT/forward iptables rules so guests can reach the
// internet via the host's egress interface. The package shells out to the
// standard `ip`/`iptables` binaries rather than vendoring netlink — fewer
// dependencies and the operations are infrequent enough that the exec cost
// doesn't matter.
package network

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type Config struct {
	Bridge      string // e.g. "hyperfleet0"
	Subnet      string // CIDR, e.g. "10.42.0.0/16"
	Gateway     string // e.g. "10.42.0.1"
	EgressIface string // optional; autodetected from default route if empty
	DNS         string // resolver written into the guest, e.g. "1.1.1.1"
}

// Allocation is everything a VM needs to be wired into the bridge and given a
// working IPv4 stack inside the guest.
type Allocation struct {
	IP         net.IP
	Mask       net.IPMask
	Gateway    net.IP
	DNS        string
	TapName    string
	MacAddress string
}

type Manager struct {
	cfg     Config
	subnet  *net.IPNet
	gateway net.IP
	egress  string

	mu        sync.Mutex
	allocated map[string]string // ip → machine id
}

func New(cfg Config) (*Manager, error) {
	if cfg.Bridge == "" {
		return nil, errors.New("bridge name required")
	}
	_, subnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return nil, fmt.Errorf("parse subnet %q: %w", cfg.Subnet, err)
	}
	gw := net.ParseIP(cfg.Gateway)
	if gw == nil || gw.To4() == nil {
		return nil, fmt.Errorf("invalid gateway %q", cfg.Gateway)
	}
	if !subnet.Contains(gw) {
		return nil, fmt.Errorf("gateway %s is outside subnet %s", gw, subnet)
	}
	if cfg.DNS == "" {
		cfg.DNS = "1.1.1.1"
	}

	m := &Manager{
		cfg:       cfg,
		subnet:    subnet,
		gateway:   gw.To4(),
		allocated: make(map[string]string),
	}
	m.allocated[m.gateway.String()] = "__gateway__"
	return m, nil
}

// Setup ensures the bridge exists with the gateway IP, IP forwarding is on,
// and NAT/forward rules are installed. Idempotent.
func (m *Manager) Setup() error {
	if err := ensureBridge(m.cfg.Bridge, m.gateway, m.subnet); err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	if err := enableForwarding(); err != nil {
		return fmt.Errorf("ip_forward: %w", err)
	}
	egress := m.cfg.EgressIface
	if egress == "" {
		eg, err := detectEgress()
		if err != nil {
			return fmt.Errorf("detect egress iface: %w", err)
		}
		egress = eg
	}
	m.egress = egress
	if err := ensureIptables(m.cfg.Subnet, m.cfg.Bridge, egress); err != nil {
		return fmt.Errorf("iptables: %w", err)
	}
	return nil
}

// Allocate picks a free IP in the subnet, creates a tap device for the given
// machine, attaches it to the bridge, brings it up, and returns the bundle.
// Caller is expected to call Release with the same machine id when the VM exits.
func (m *Manager) Allocate(machineID string) (Allocation, error) {
	m.mu.Lock()
	ip, err := m.pickIPLocked(machineID)
	m.mu.Unlock()
	if err != nil {
		return Allocation{}, err
	}

	tap := tapName(machineID)
	mac, err := randomMAC()
	if err != nil {
		m.releaseIP(ip)
		return Allocation{}, fmt.Errorf("mac: %w", err)
	}
	if err := createTap(tap, m.cfg.Bridge); err != nil {
		m.releaseIP(ip)
		return Allocation{}, fmt.Errorf("tap: %w", err)
	}

	return Allocation{
		IP:         ip,
		Mask:       m.subnet.Mask,
		Gateway:    m.gateway,
		DNS:        m.cfg.DNS,
		TapName:    tap,
		MacAddress: mac,
	}, nil
}

func (m *Manager) Egress() string { return m.egress }

func (m *Manager) Release(machineID string, a Allocation) {
	if a.TapName != "" {
		_ = run("ip", "link", "del", a.TapName)
	}
	if a.IP != nil {
		m.releaseIP(a.IP)
	}
}

func (m *Manager) releaseIP(ip net.IP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.allocated, ip.String())
}

func (m *Manager) pickIPLocked(machineID string) (net.IP, error) {
	// Linear scan from .2 upwards, skipping .0 (network) and gateway.
	base := m.subnet.IP.To4()
	mask := m.subnet.Mask
	ones, bits := mask.Size()
	hostBits := bits - ones
	if hostBits < 2 {
		return nil, errors.New("subnet too small")
	}
	total := uint32(1) << uint32(hostBits)
	for i := uint32(2); i < total-1; i++ { // skip network (.0), .1 reserved-ish, broadcast (last)
		ip := offsetIP(base, i)
		if ip.Equal(m.gateway) {
			continue
		}
		key := ip.String()
		if _, busy := m.allocated[key]; busy {
			continue
		}
		m.allocated[key] = machineID
		return ip, nil
	}
	return nil, errors.New("subnet exhausted")
}

func offsetIP(base net.IP, n uint32) net.IP {
	ip := make(net.IP, 4)
	copy(ip, base.To4())
	v := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	v += n
	ip[0] = byte(v >> 24)
	ip[1] = byte(v >> 16)
	ip[2] = byte(v >> 8)
	ip[3] = byte(v)
	return ip
}

// tapName derives a tap device name short enough to fit IFNAMSIZ (15).
// "hf-" + first 12 chars of the machine ID.
func tapName(id string) string {
	short := id
	if len(short) > 12 {
		short = short[:12]
	}
	return "hf-" + short
}

func randomMAC() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Locally administered, unicast.
	b[0] = (b[0] | 0x02) &^ 0x01
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5]), nil
}

func ensureBridge(name string, gw net.IP, subnet *net.IPNet) error {
	if err := run("ip", "link", "show", name); err != nil {
		if err := run("ip", "link", "add", name, "type", "bridge"); err != nil {
			return fmt.Errorf("add bridge: %w", err)
		}
	}
	ones, _ := subnet.Mask.Size()
	addr := fmt.Sprintf("%s/%d", gw, ones)
	// Best-effort: ignore "File exists" if address already there.
	_ = run("ip", "addr", "add", addr, "dev", name)
	if err := run("ip", "link", "set", name, "up"); err != nil {
		return fmt.Errorf("bring bridge up: %w", err)
	}
	return nil
}

func createTap(name, bridge string) error {
	// If a stale tap with this name exists from a crashed daemon, remove it.
	_ = run("ip", "link", "del", name)
	if err := run("ip", "tuntap", "add", "dev", name, "mode", "tap"); err != nil {
		return fmt.Errorf("add tap: %w", err)
	}
	if err := run("ip", "link", "set", name, "master", bridge); err != nil {
		_ = run("ip", "link", "del", name)
		return fmt.Errorf("attach to bridge: %w", err)
	}
	if err := run("ip", "link", "set", name, "up"); err != nil {
		_ = run("ip", "link", "del", name)
		return fmt.Errorf("bring tap up: %w", err)
	}
	return nil
}

func enableForwarding() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644)
}

func ensureIptables(subnet, bridge, egress string) error {
	rules := [][]string{
		{"-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-o", egress, "-j", "MASQUERADE"},
		{"-A", "FORWARD", "-i", bridge, "-o", egress, "-j", "ACCEPT"},
		{"-A", "FORWARD", "-i", egress, "-o", bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, r := range rules {
		check := append([]string(nil), r...)
		// swap "-A" for "-C" to test for existence
		for i, tok := range check {
			if tok == "-A" {
				check[i] = "-C"
				break
			}
		}
		if err := run("iptables", check...); err == nil {
			continue
		}
		if err := run("iptables", r...); err != nil {
			return fmt.Errorf("install rule %v: %w", r, err)
		}
	}
	return nil
}

func detectEgress() (string, error) {
	out, err := exec.Command("ip", "-o", "route", "get", "1.1.1.1").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ip route get: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// "1.1.1.1 via 192.168.1.1 dev wlp1s0 src 192.168.1.42 uid 0 \n cache"
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("no dev in: %s", strings.TrimSpace(string(out)))
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
