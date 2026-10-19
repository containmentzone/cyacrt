package box

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// run executes a command and folds its output into the error.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// nftRules is replaced atomically on every EnsureNetwork. Boxes can reach
// the internet through NAT and nothing else: not the host, not each other
// (bridge port isolation handles that at L2), not private or link-local
// ranges. 169.254.0.0/16 stays reserved for future host services.
const nftRules = `
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
		ip saddr 10.88.0.0/16 oifname != "cyac0" masquerade
	}
}
`

// EnsureNetwork creates the bridge and firewall rules if needed. Idempotent.
func EnsureNetwork() error {
	if run("ip", "link", "show", BridgeName) != nil {
		if err := run("ip", "link", "add", BridgeName, "type", "bridge"); err != nil {
			return err
		}
		if err := run("ip", "addr", "add", fmt.Sprintf("%s/%d", HostIP, PrefixLen), "dev", BridgeName); err != nil {
			return err
		}
	}
	if err := run("ip", "link", "set", BridgeName, "up"); err != nil {
		return err
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return err
	}
	nft := exec.Command("nft", "-f", "-")
	nft.Stdin = strings.NewReader(nftRules)
	if out, err := nft.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, out)
	}
	return nil
}

// attach puts a host-side interface on the bridge with port isolation on,
// so isolated ports can only talk to the bridge itself (the host), not to
// each other.
func attach(ifname string) error {
	if err := run("ip", "link", "set", ifname, "master", BridgeName); err != nil {
		return err
	}
	if err := run("ip", "link", "set", "dev", ifname, "type", "bridge_slave", "isolated", "on"); err != nil {
		return err
	}
	return run("ip", "link", "set", ifname, "up")
}

// addTap creates the tap device Firecracker uses as the VM's eth0.
func addTap(b *Box) error {
	if err := run("ip", "tuntap", "add", "dev", b.ifName(), "mode", "tap"); err != nil {
		return err
	}
	return attach(b.ifName())
}

func netnsName(b *Box) string { return "cyac-" + b.Name }

// addNetns creates a named netns with eth0 configured, for the runc tier.
// The address is also in the disk's networkd config, so it stays put if
// systemd-networkd reconfigures eth0.
func addNetns(b *Box) (path string, err error) {
	ns := netnsName(b)
	if err := run("ip", "netns", "add", ns); err != nil {
		return "", err
	}
	steps := [][]string{
		{"link", "add", b.ifName(), "type", "veth", "peer", "name", "eth0", "netns", ns},
		{"-n", ns, "link", "set", "lo", "up"},
		{"-n", ns, "addr", "add", fmt.Sprintf("%s/%d", b.IP(), PrefixLen), "dev", "eth0"},
		{"-n", ns, "link", "set", "eth0", "up"},
		{"-n", ns, "route", "add", "default", "via", HostIP.String()},
	}
	for _, args := range steps {
		if err := run("ip", args...); err != nil {
			return "", err
		}
	}
	return "/run/netns/" + ns, attach(b.ifName())
}

// removeNet tears down whatever addTap or addNetns created. Errors are
// ignored because either may be half-done or already gone.
func removeNet(b *Box) {
	run("ip", "link", "del", b.ifName())
	run("ip", "netns", "del", netnsName(b))
}
