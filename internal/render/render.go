// Package render turns one object from the conf into the plan its unit
// carries out: the commands that create its kernel objects, and the ones
// that remove them. It is pure, like conf and units, so every plan can be
// read and tested as text; package run is what executes them.
//
// Each plan owns its objects outright. Nothing is shared between units:
// a network owns its bridge and its leg into its gateway, a service owns
// its netns and every leg it has, a box owns its port, and each owns its
// own nft tables. That is what lets Down be "delete what I own".
package render

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/containmentzone/cyacrt/internal/conf"
)

// Step is one command: argv, and what to feed its stdin (nft scripts).
type Step struct {
	Args  []string
	Stdin string
}

// Plan is what a unit does. Up runs in order and stops at the first
// error. Down runs every step and ignores errors, so it is safe after a
// half-finished Up or none at all.
type Plan struct {
	Up, Down []Step
}

func (p *Plan) up(args ...string)   { p.Up = append(p.Up, Step{Args: args}) }
func (p *Plan) down(args ...string) { p.Down = append(p.Down, Step{Args: args}) }

// nft loads table atomically, replacing an older version of it.
func (p *Plan) nft(family, name, body string) {
	script := fmt.Sprintf("table %[1]s %[2]s\ndelete table %[1]s %[2]s\ntable %[1]s %[2]s {\n%[3]s}\n", family, name, body)
	p.Up = append(p.Up, Step{Args: []string{"nft", "-f", "-"}, Stdin: script})
	p.down("nft", "delete", "table", family, name)
}

// in runs args inside netns ns.
func in(ns string, args ...string) []string {
	return append([]string{"ip", "netns", "exec", ns}, args...)
}

// Names of kernel objects. Interface names are capped at 15 bytes, so
// they are built from indexes and addresses, never from object names;
// netns and nft table names have no such limit.
func GatewayNetns(g string) string { return "cygw-" + g }
func ServiceNetns(s string) string { return "cysvc-" + s }
func BoxNetns(b string) string     { return "cybox-" + b }

// Bridge is network n's bridge.
func Bridge(n *conf.Network) string { return fmt.Sprintf("cyn%d", n.Index) }

// Port is the host end of a box's link: a tap for a VM, or the host end
// of a veth for runc.
func Port(b *conf.Box) string {
	a := b.Address.As4()
	return fmt.Sprintf("cyb%d-%d", a[2], a[3])
}

// MAC is the address of the machine at addr. It is fixed by the address,
// so the anti-spoofing rules can name it before the machine exists.
func MAC(addr netip.Addr) string {
	a := addr.As4()
	return fmt.Sprintf("02:c7:ac:00:%02x:%02x", a[2], a[3])
}

// private is what no gateway forwards to: other hosts' LANs, CGNAT,
// link-local (cloud metadata), cyac's own range, Azure's WireServer
// (168.63.129.16, which hands out the VM's goal state and certificates),
// and the special-use ranges that are never the internet: "this network",
// loopback, IETF protocol assignments, benchmarking, multicast and
// reserved (with broadcast).
const private = "{ 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 168.63.129.16, 169.254.0.0/16, " +
	"172.16.0.0/12, 192.0.0.0/24, 192.168.0.0/16, 198.18.0.0/15, 224.0.0.0/4, 240.0.0.0/4 }"

// Gateway is the plan for cygateway@<g>: its netns, its uplink, and the
// firewall that lets legs out through the uplink and nowhere else.
func Gateway(g *conf.Gateway) Plan {
	var p Plan
	ns := GatewayNetns(g.Name)
	p.up("ip", "netns", "add", ns)
	p.up("ip", "-n", ns, "link", "set", "lo", "up")
	p.up(in(ns, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")...)

	var uplink string
	switch g.Kind {
	case "direct":
		// A /31 to the host, which masquerades it once more out of
		// whatever the host's own default route is.
		uplink = "uplink"
		host, gw := g.Uplink()
		hostIf := fmt.Sprintf("cygw%d", g.Index)
		p.up("ip", "link", "add", hostIf, "type", "veth", "peer", "name", uplink, "netns", ns)
		p.up("ip", "addr", "add", host.String(), "dev", hostIf)
		p.up("ip", "link", "set", hostIf, "up")
		p.up("ip", "-n", ns, "addr", "add", gw.String(), "dev", uplink)
		p.up("ip", "-n", ns, "link", "set", uplink, "up")
		p.up("ip", "-n", ns, "route", "add", "default", "via", host.Addr().String())
		p.up("sysctl", "-q", "-w", "net.ipv4.ip_forward=1")
		p.nft("inet", "cygw-"+g.Name, fmt.Sprintf(`	chain input {
		type filter hook input priority 0; policy accept;
		iifname %[1]q ct state established,related accept
		iifname %[1]q drop
	}
	chain forward {
		type filter hook forward priority 0; policy accept;
%[3]s		iifname %[1]q accept
		oifname %[1]q ct state established,related accept
	}
	chain postrouting {
		type nat hook postrouting priority 100;
		ip saddr %[2]s oifname != %[1]q masquerade
	}
`, hostIf, gw.Addr(), hostForward(g, hostIf)))
		p.down("ip", "link", "del", hostIf)
	case "wireguard":
		// Created in the host netns so its UDP socket stays there and
		// encrypted packets use the host's routes, then moved into the
		// gateway: the only way out of the netns is the tunnel.
		uplink = "wg0"
		tmp := fmt.Sprintf("cywg%d", g.Index)
		p.up("ip", "link", "add", tmp, "type", "wireguard")
		p.up("ip", "link", "set", tmp, "netns", ns)
		p.up("ip", "-n", ns, "link", "set", tmp, "name", uplink)
		p.up(in(ns, "wg", "setconf", uplink, g.Config)...)
		p.up("ip", "-n", ns, "addr", "add", g.Address.String(), "dev", uplink)
		p.up("ip", "-n", ns, "link", "set", uplink, "up")
		p.up("ip", "-n", ns, "route", "add", "default", "dev", uplink)
		p.down("ip", "link", "del", tmp) // only if a failed Up left it in the host netns
	}

	// Legs come and go with the networks and services that own them; the
	// rules only name the uplink, so they never change.
	p.Up = append(p.Up, Step{Args: in(ns, "nft", "-f", "-"), Stdin: fmt.Sprintf(`table inet gw {
	chain input {
		type filter hook input priority 0; policy drop;
		iifname "lo" accept
		ct state established,related accept
	}
	chain forward {
		type filter hook forward priority 0; policy drop;
		ct state established,related accept
		oifname %[1]q ip daddr %[2]s drop
		oifname %[1]q accept
	}
	chain postrouting {
		type nat hook postrouting priority 100;
		oifname %[1]q masquerade
	}
}
`, uplink, private)})
	// Deleting the netns deletes everything in it, the legs' ends too.
	p.down("ip", "netns", "del", ns)
	return p
}

// hostForward is what the host drops of a direct gateway's traffic before
// forwarding the rest: the same private ranges the gateway already drops,
// in case its netns rules are gone, and, with Uplink=, every interface
// but that one. Its resolver (DNS=, which may well be on the LAN) stays
// reachable: only cyac dns asks it from inside the gateway, as the
// gateway's own rules drop the private range for boxes.
func hostForward(g *conf.Gateway, hostIf string) string {
	var b strings.Builder
	if g.DNS != "" {
		fmt.Fprintf(&b, "\t\tiifname %q ip daddr %s meta l4proto { tcp, udp } th dport 53 accept\n", hostIf, g.DNS)
	}
	fmt.Fprintf(&b, "\t\tiifname %q ip daddr %s drop\n", hostIf, private)
	if g.Out != "" {
		fmt.Fprintf(&b, "\t\tiifname %q oifname != %q drop\n", hostIf, g.Out)
	}
	return b.String()
}

// Network is the plan for cynetwork@<n>: the bridge with the host on .2,
// a host firewall that lets members reach the host's DNS and nothing
// else, and the leg into the gateway (.1), if there is one.
func Network(n *conf.Network) Plan {
	var p Plan
	br := Bridge(n)
	p.up("ip", "link", "add", br, "type", "bridge")
	p.up("ip", "addr", "add", netip.PrefixFrom(n.Host(), 24).String(), "dev", br)
	p.up("ip", "link", "set", br, "up")
	p.nft("inet", "cyn-"+n.Name, fmt.Sprintf(`	chain input {
		type filter hook input priority 0; policy accept;
		iifname %[1]q ct state established,related accept
		iifname %[1]q ip daddr %[2]s udp dport 53 accept
		iifname %[1]q ip daddr %[2]s tcp dport 53 accept
		iifname %[1]q drop
	}
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname %[1]q drop
		oifname %[1]q drop
	}
`, br, n.Host()))
	if n.Gateway != "" {
		ns := GatewayNetns(n.Gateway)
		hostIf := fmt.Sprintf("cynl%d", n.Index)
		leg := fmt.Sprintf("n%d", n.Index)
		p.up("ip", "link", "add", hostIf, "type", "veth", "peer", "name", leg, "netns", ns)
		p.up("ip", "link", "set", hostIf, "master", br)
		p.up("ip", "link", "set", hostIf, "up")
		p.up("ip", "-n", ns, "addr", "add", netip.PrefixFrom(n.Router(), 24).String(), "dev", leg)
		p.up("ip", "-n", ns, "link", "set", leg, "up")
		p.down("ip", "link", "del", hostIf)
	}
	p.down("ip", "link", "del", br)
	return p
}

// pin is the bridge-family rules that hold the port ifname to one MAC and
// one IPv4 address, so a machine's source address is its identity.
func pin(ifname string, addr netip.Addr) string {
	return fmt.Sprintf(`		iifname %[1]q ether saddr != %[2]s drop
		iifname %[1]q ether type ip ip saddr != %[3]s drop
		iifname %[1]q ether type arp arp saddr ip != %[3]s drop
		iifname %[1]q ether type ip6 drop
`, ifname, MAC(addr), addr)
}

// pinTable puts rules on both bridge hooks: forward for frames to other
// ports, input for frames to the host itself.
func pinTable(rules string) string {
	return "\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n" + rules +
		"\t}\n\tchain input {\n\t\ttype filter hook input priority 0; policy accept;\n" + rules + "\t}\n"
}

// Service is the plan for cyservice@<s>'s plumbing: a netns with a leg on
// every network that attaches it, a /31 into its gateway as the default
// route, and no forwarding between any of them. The container is started
// in this netns afterwards.
func Service(c *conf.Config, s *conf.Service) Plan {
	var p Plan
	ns := ServiceNetns(s.Name)
	p.up("ip", "netns", "add", ns)
	p.up("ip", "-n", ns, "link", "set", "lo", "up")

	var rules strings.Builder
	for _, name := range c.AttachedBy(s.Name) {
		n := c.Networks[name]
		addr := s.AddressIn(n)
		hostIf := fmt.Sprintf("cys%dn%d", s.Index, n.Index)
		leg := fmt.Sprintf("n%d", n.Index)
		p.up("ip", "link", "add", hostIf, "type", "veth", "peer", "name", leg, "netns", ns)
		p.up("ip", "-n", ns, "link", "set", leg, "address", MAC(addr))
		p.up("ip", "-n", ns, "addr", "add", netip.PrefixFrom(addr, 24).String(), "dev", leg)
		p.up("ip", "-n", ns, "link", "set", leg, "up")
		p.up("ip", "link", "set", hostIf, "master", Bridge(n))
		p.up("ip", "link", "set", hostIf, "up")
		rules.WriteString(pin(hostIf, addr))
	}
	if rules.Len() > 0 {
		p.nft("bridge", "cys-"+s.Name, pinTable(rules.String()))
	}

	gwNs := GatewayNetns(s.Gateway)
	gwEnd, svcEnd := s.Link()
	gwIf := fmt.Sprintf("s%d", s.Index)
	p.up("ip", "-n", ns, "link", "add", "gw", "type", "veth", "peer", "name", gwIf, "netns", gwNs)
	p.up("ip", "-n", ns, "addr", "add", svcEnd.String(), "dev", "gw")
	p.up("ip", "-n", ns, "link", "set", "gw", "up")
	p.up("ip", "-n", gwNs, "addr", "add", gwEnd.String(), "dev", gwIf)
	p.up("ip", "-n", gwNs, "link", "set", gwIf, "up")
	p.up("ip", "-n", ns, "route", "add", "default", "via", gwEnd.Addr().String())
	// A new netns doesn't forward; this keeps it that way even if the
	// container could change the sysctl.
	p.Up = append(p.Up, Step{Args: in(ns, "nft", "-f", "-"),
		Stdin: "table inet svc {\n\tchain forward {\n\t\ttype filter hook forward priority 0; policy drop;\n\t}\n}\n"})

	// The netns takes its legs, and their host ends, with it.
	p.down("ip", "netns", "del", ns)
	return p
}

// Box is the plan for cybox@<b>'s link. A VM gets a tap on the bridge
// (its MAC goes into the VMM config); a runc box gets a netns with eth0
// configured. tap says which.
func Box(c *conf.Config, b *conf.Box, tap bool) Plan {
	var p Plan
	n := c.Networks[b.Network]
	port := Port(b)
	if tap {
		p.up("ip", "tuntap", "add", "dev", port, "mode", "tap")
	} else {
		ns := BoxNetns(b.Name)
		p.up("ip", "netns", "add", ns)
		p.up("ip", "link", "add", port, "type", "veth", "peer", "name", "eth0", "netns", ns)
		p.up("ip", "-n", ns, "link", "set", "lo", "up")
		p.up("ip", "-n", ns, "link", "set", "eth0", "address", MAC(b.Address))
		p.up("ip", "-n", ns, "addr", "add", netip.PrefixFrom(b.Address, 24).String(), "dev", "eth0")
		p.up("ip", "-n", ns, "link", "set", "eth0", "up")
		if n.Gateway != "" {
			p.up("ip", "-n", ns, "route", "add", "default", "via", n.Router().String())
		}
		p.down("ip", "netns", "del", ns)
	}
	p.up("ip", "link", "set", port, "master", Bridge(n))
	p.up("ip", "link", "set", port, "up")
	p.nft("bridge", "cyb-"+b.Name, pinTable(pin(port, b.Address)))
	p.down("ip", "link", "del", port)
	return p
}

// Networkd is the systemd-networkd config cyac writes into the box for
// eth0 on every start. A sealed network gets no default route, and DNS is
// always the host's responder on .2.
func Networkd(c *conf.Config, b *conf.Box) string {
	n := c.Networks[b.Network]
	var s strings.Builder
	s.WriteString("# written by cyac on every start\n[Match]\nName=eth0\n\n[Network]\n")
	fmt.Fprintf(&s, "Address=%s\n", netip.PrefixFrom(b.Address, 24))
	if n.Gateway != "" {
		fmt.Fprintf(&s, "Gateway=%s\n", n.Router())
	}
	fmt.Fprintf(&s, "DNS=%s\nDomains=%s\n", n.Host(), Domain(n))
	return s.String()
}

// Domain is network n's DNS zone, which boxes also search.
func Domain(n *conf.Network) string { return n.Name + ".cyac.internal" }
