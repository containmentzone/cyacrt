// Package bridge is the default network driver: one Linux bridge with
// port isolation, static addresses, and nftables NAT out.
package bridge

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"

	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/sh"
)

// Layout: bridge cyac0 on 10.42.0.0/16, host at 10.42.0.1, box id N at
// 10.42.N/256.N%256.
const (
	bridgeName = "cyac0"
	prefixLen  = 16
)

var hostIP = netip.MustParseAddr("10.42.0.1")

// rules is replaced atomically on every Setup. Boxes can reach the internet
// through NAT and nothing else: not the host, not each other (bridge port
// isolation handles that at L2), not private or link-local ranges.
// 169.254.0.0/16 stays reserved for future host services.
const rules = `
table inet cyac
delete table inet cyac
table inet cyac {
	chain input {
		type filter hook input priority 0; policy accept;
		iifname "cyac0" ct state established,related accept
		iifname "cyac0" drop
	}
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname "cyac0" ct state established,related accept
		iifname "cyac0" ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 100.64.0.0/10 } drop
		iifname "cyac0" accept
	}
	chain postrouting {
		type nat hook postrouting priority 100;
		ip saddr 10.42.0.0/16 oifname != "cyac0" masquerade
	}
}
`

// Network implements driver.Network.
type Network struct {
	DNS string // resolver written into each box's config
}

var _ driver.Network = Network{}

func (n Network) Addr(id int) driver.Addr {
	return driver.Addr{
		IP:      netip.AddrFrom4([4]byte{10, 42, byte(id >> 8), byte(id)}),
		Prefix:  prefixLen,
		Gateway: hostIP,
		DNS:     n.DNS,
	}
}

func (Network) Setup() error {
	if sh.Run("ip", "link", "show", bridgeName) != nil {
		if err := sh.Run("ip", "link", "add", bridgeName, "type", "bridge"); err != nil {
			return err
		}
		if err := sh.Run("ip", "addr", "add", fmt.Sprintf("%s/%d", hostIP, prefixLen), "dev", bridgeName); err != nil {
			return err
		}
	}
	if err := sh.Run("ip", "link", "set", bridgeName, "up"); err != nil {
		return err
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return err
	}
	nft := exec.Command("nft", "-f", "-")
	nft.Stdin = strings.NewReader(rules)
	if out, err := nft.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, out)
	}
	return nil
}

// Teardown deletes the bridge and the nftables table. The daemon calls it
// on start and exit, so a bridge from an older layout (cyac used
// 10.88.0.0/16 before) or a crashed daemon never outlives it.
func (Network) Teardown() {
	sh.Run("ip", "link", "del", bridgeName)
	sh.Run("nft", "delete", "table", "inet", "cyac")
}

func (n Network) Attach(m *driver.Machine, kind driver.LinkKind) error {
	switch kind {
	case driver.Tap:
		return n.attachTap(m)
	case driver.Netns:
		return n.attachNetns(m)
	}
	return fmt.Errorf("bridge: unsupported link kind %d", kind)
}

func (Network) Detach(m *driver.Machine) {
	sh.Run("ip", "link", "del", ifName(m))
	sh.Run("ip", "netns", "del", netnsName(m))
}

// ifName is the host-side interface; Linux caps names at 15 bytes.
func ifName(m *driver.Machine) string    { return fmt.Sprintf("cyac-%d", m.ID) }
func netnsName(m *driver.Machine) string { return "cyac-" + m.Name }

func (Network) attachTap(m *driver.Machine) error {
	if err := sh.Run("ip", "tuntap", "add", "dev", ifName(m), "mode", "tap"); err != nil {
		return err
	}
	m.Link = driver.Link{HostIface: ifName(m)}
	return plug(ifName(m))
}

// attachNetns creates a named netns with eth0 configured. The address is
// also in the disk's networkd config, so it stays put if systemd-networkd
// reconfigures eth0.
func (Network) attachNetns(m *driver.Machine) error {
	ns := netnsName(m)
	if err := sh.Run("ip", "netns", "add", ns); err != nil {
		return err
	}
	steps := [][]string{
		{"link", "add", ifName(m), "type", "veth", "peer", "name", "eth0", "netns", ns},
		{"-n", ns, "link", "set", "lo", "up"},
		{"-n", ns, "addr", "add", fmt.Sprintf("%s/%d", m.Addr.IP, m.Addr.Prefix), "dev", "eth0"},
		{"-n", ns, "link", "set", "eth0", "up"},
		{"-n", ns, "route", "add", "default", "via", m.Addr.Gateway.String()},
	}
	for _, args := range steps {
		if err := sh.Run("ip", args...); err != nil {
			return err
		}
	}
	m.Link = driver.Link{HostIface: ifName(m), Netns: "/run/netns/" + ns}
	return plug(ifName(m))
}

// plug puts a host-side interface on the bridge with port isolation on, so
// isolated ports can only talk to the bridge itself (the host), not to each
// other.
func plug(ifname string) error {
	if err := sh.Run("ip", "link", "set", ifname, "master", bridgeName); err != nil {
		return err
	}
	if err := sh.Run("ip", "link", "set", "dev", ifname, "type", "bridge_slave", "isolated", "on"); err != nil {
		return err
	}
	return sh.Run("ip", "link", "set", ifname, "up")
}
