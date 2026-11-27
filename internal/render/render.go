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
// A step with IfExists runs only if that path exists, and its failure
// doesn't fail the plan but skips the rest of the steps with that path: that is how a network or gateway that comes back
// puts the legs of workloads that kept running back in place (see
// rejoin), as far as they still fit.
type Step struct {
	Args     []string
	Stdin    string
	IfExists string `json:",omitempty"`
}

// Plan is what a unit does. Up runs in order and stops at the first
// error. Down runs every step and ignores errors, so it is safe after a
// half-finished Up or none at all.
type Plan struct {
	Up, Down []Step
	ifExists string // set by rejoin while it adds steps
}

func (p *Plan) up(args ...string)   { p.Up = append(p.Up, Step{Args: args, IfExists: p.ifExists}) }
func (p *Plan) down(args ...string) { p.Down = append(p.Down, Step{Args: args}) }

// rejoin adds the steps add makes, each to run only if path exists.
func (p *Plan) rejoin(path string, add func()) {
	p.ifExists = path
	add()
	p.ifExists = ""
}

func linkPath(ifname string) string { return "/sys/class/net/" + ifname }

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
// firewall that lets legs out through the uplink and nowhere else. If it
// comes back while networks and services on it kept running, it puts
// their legs back: they died with its old netns.
func Gateway(c *conf.Config, g *conf.Gateway) Plan {
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
		iifname != %[1]q meta l4proto { tcp, udp } th dport 53 accept
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
	for _, name := range conf.Sorted(c.Networks) {
		if n := c.Networks[name]; n.Gateway == g.Name {
			p.rejoin(linkPath(Bridge(n)), func() { networkLeg(&p, n) })
		}
	}
	for _, name := range conf.Sorted(c.Services) {
		if s := c.Services[name]; s.Gateway == g.Name {
			p.rejoin(NetnsPath(ServiceNetns(s.Name)), func() { serviceLeg(&p, s) })
		}
	}
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
// a host firewall that lets members answer the host (the proxy) and
// reach nothing on it, and the leg into the gateway (.1), if there is one. If it comes
// back while boxes and services on it kept running, it puts their ports,
// which outlive the bridge, back on it.
func Network(c *conf.Config, n *conf.Network) Plan {
	var p Plan
	br := Bridge(n)
	// A fixed MAC: a bridge without one takes its lowest port's, so it
	// would change whenever a service's leg comes or goes, and every
	// box's ARP entry for the host on .2 (the proxy) would go stale with it.
	p.up("ip", "link", "add", br, "address", MAC(n.Host()), "type", "bridge")
	p.up("ip", "addr", "add", netip.PrefixFrom(n.Host(), 24).String(), "dev", br)
	p.up("ip", "link", "set", br, "up")
	// Down deletes the bridge first, so its ports, which may outlive it,
	// never sit on it without the host firewall below. Off it, their own
	// tables keep them shut (see detached).
	p.down("ip", "link", "del", br)
	p.nft("inet", "cyn-"+n.Name, fmt.Sprintf(`	chain input {
		type filter hook input priority 0; policy accept;
		iifname %[1]q ct state established,related accept
		iifname %[1]q drop
	}
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname %[1]q drop
		oifname %[1]q drop
	}
`, br))
	if n.Gateway != "" {
		networkLeg(&p, n)
		p.down("ip", "link", "del", networkLegName(n))
	}
	var ports []string
	for _, name := range conf.Sorted(c.Boxes) {
		if b := c.Boxes[name]; b.Network == n.Name {
			ports = append(ports, Port(b))
		}
	}
	for _, s := range n.Services {
		ports = append(ports, serviceLegName(c.Services[s], n))
	}
	for _, port := range ports {
		p.rejoin(linkPath(port), func() {
			p.up("ip", "link", "set", port, "master", br)
			p.up("ip", "link", "set", port, "up")
		})
	}
	return p
}

// networkLeg is network n's leg into its gateway, the gateway's .1 on it.
func networkLeg(p *Plan, n *conf.Network) {
	ns := GatewayNetns(n.Gateway)
	hostIf, leg := networkLegName(n), fmt.Sprintf("n%d", n.Index)
	p.up("ip", "link", "add", hostIf, "type", "veth", "peer", "name", leg, "netns", ns)
	p.up("ip", "link", "set", hostIf, "master", Bridge(n))
	p.up("ip", "link", "set", hostIf, "up")
	p.up("ip", "-n", ns, "addr", "add", netip.PrefixFrom(n.Router(), 24).String(), "dev", leg)
	p.up("ip", "-n", ns, "link", "set", leg, "up")
}

func networkLegName(n *conf.Network) string { return fmt.Sprintf("cynl%d", n.Index) }

// pin is the bridge-family rules that hold the port ifname to one MAC and
// one IPv4 address, so a machine's source address is its identity.
func pin(ifname string, addr netip.Addr) string {
	return fmt.Sprintf(`		iifname %[1]q ether saddr != %[2]s drop
		iifname %[1]q ether type ip ip saddr != %[3]s drop
		iifname %[1]q ether type arp arp saddr ip != %[3]s drop
		iifname %[1]q ether type ip6 drop
`, ifname, MAC(addr), addr)
}

// detached is the host rules for ports while they are on no bridge: a
// network that stops leaves its machines' ports up, and the host must not
// take or route anything from them until the network puts them back. On
// a bridge, traffic comes in on the bridge's name, so this never matches.
func detached(ports ...string) string {
	set := `"` + strings.Join(ports, `", "`) + `"`
	return fmt.Sprintf(`	chain input {
		type filter hook input priority 0; policy accept;
		iifname { %[1]s } drop
	}
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname { %[1]s } drop
	}
`, set)
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
	var legs []string
	for _, name := range c.AttachedBy(s.Name) {
		n := c.Networks[name]
		addr := s.AddressIn(n)
		hostIf := serviceLegName(s, n)
		leg := fmt.Sprintf("n%d", n.Index)
		p.up("ip", "link", "add", hostIf, "type", "veth", "peer", "name", leg, "netns", ns)
		p.up("ip", "-n", ns, "link", "set", leg, "address", MAC(addr))
		p.up("ip", "-n", ns, "addr", "add", netip.PrefixFrom(addr, 24).String(), "dev", leg)
		p.up("ip", "-n", ns, "link", "set", leg, "up")
		p.up("ip", "link", "set", hostIf, "master", Bridge(n))
		p.up("ip", "link", "set", hostIf, "up")
		rules.WriteString(pin(hostIf, addr))
		legs = append(legs, hostIf)
	}
	if rules.Len() > 0 {
		p.nft("bridge", "cys-"+s.Name, pinTable(rules.String()))
		p.nft("inet", "cys-"+s.Name, detached(legs...))
	}

	serviceLeg(&p, s)
	// A new netns doesn't forward; this keeps it that way even if the
	// container could change the sysctl.
	p.Up = append(p.Up, Step{Args: in(ns, "nft", "-f", "-"),
		Stdin: "table inet svc {\n\tchain forward {\n\t\ttype filter hook forward priority 0; policy drop;\n\t}\n}\n"})

	// The netns takes its legs, and their host ends, with it.
	p.down("ip", "netns", "del", ns)
	return p
}

// serviceLeg is service s's /31 into its gateway, and its default route.
func serviceLeg(p *Plan, s *conf.Service) {
	ns, gwNs := ServiceNetns(s.Name), GatewayNetns(s.Gateway)
	gwEnd, svcEnd := s.Link()
	gwIf := fmt.Sprintf("s%d", s.Index)
	p.up("ip", "-n", ns, "link", "add", "gw", "type", "veth", "peer", "name", gwIf, "netns", gwNs)
	p.up("ip", "-n", ns, "addr", "add", svcEnd.String(), "dev", "gw")
	p.up("ip", "-n", ns, "link", "set", "gw", "up")
	p.up("ip", "-n", gwNs, "addr", "add", gwEnd.String(), "dev", gwIf)
	p.up("ip", "-n", gwNs, "link", "set", gwIf, "up")
	p.up("ip", "-n", ns, "route", "add", "default", "via", gwEnd.Addr().String())
}

// serviceLegName is the host end of service s's leg on network n, named
// after its address there like a box's port.
func serviceLegName(s *conf.Service, n *conf.Network) string {
	a := s.AddressIn(n).As4()
	return fmt.Sprintf("cys%d-%d", a[2], a[3])
}

// NetnsPath is where `ip netns` keeps netns ns, which holds it open.
func NetnsPath(ns string) string { return "/run/netns/" + ns }

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
		// No IPv6 link-local either, as networkd's LinkLocalAddressing=no
		// does in a VM; see Networkd.
		p.up("ip", "-n", ns, "link", "set", "eth0", "addrgenmode", "none")
		p.up("ip", "-n", ns, "link", "set", "eth0", "up")
		if n.Gateway != "" {
			p.up("ip", "-n", ns, "route", "add", "default", "via", n.Router().String())
		}
		p.down("ip", "netns", "del", ns)
	}
	p.up("ip", "link", "set", port, "master", Bridge(n))
	p.up("ip", "link", "set", port, "up")
	p.nft("bridge", "cyb-"+b.Name, pinTable(pin(port, b.Address)))
	p.nft("inet", "cyb-"+b.Name, detached(port))
	p.down("ip", "link", "del", port)
	return p
}

// Networkd is the systemd-networkd config cyac writes into the box for
// eth0 on every start. DNS is the gateway's resolver on the router (.1);
// a sealed network has neither. Networks are IPv4-only, so eth0 gets
// no IPv6 address at all: with even a link-local one, glibc's
// AI_ADDRCONFIG counts IPv6 as usable, and apt, curl and the like try
// every AAAA record (and fail with "Network is unreachable") first.
func Networkd(c *conf.Config, b *conf.Box) string {
	n := c.Networks[b.Network]
	var s strings.Builder
	s.WriteString("# written by cyac on every start\n[Match]\nName=eth0\n\n[Network]\n")
	fmt.Fprintf(&s, "Address=%s\n", netip.PrefixFrom(b.Address, 24))
	if n.Gateway != "" {
		// The gateway's resolver listens on the router's address. A
		// sealed network has neither.
		fmt.Fprintf(&s, "Gateway=%[1]s\nDNS=%[1]s\nDomains=%[2]s\n", n.Router(), Domain)
	}
	s.WriteString("LinkLocalAddressing=no\nIPv6AcceptRA=no\n")
	return s.String()
}

// Domain is the DNS zone the gateways' resolvers answer for, each client
// with its own network's names only, and which boxes also search.
const Domain = "cyac.internal"
