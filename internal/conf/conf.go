// Package conf reads cyac's declared state: one ini file per object in the
// conf directory, named <name>.<primitive>. The four primitives are
// cynetwork, cygateway, cyservice and cybox. These files are the only state
// cyac has; everything else (units, VM configs, kernel objects) is derived
// from them.
//
// conf is pure: it reads files and checks them, and touches nothing else.
package conf

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Dir is where the files live. It is under /var, not /etc, so the CLI can
// write it on hosts where /etc is read-only (NixOS).
const Dir = "/var/lib/cyac/conf"

// The primitives, which are also the file extensions and unit prefixes.
const (
	KindNetwork = "cynetwork"
	KindGateway = "cygateway"
	KindService = "cyservice"
	KindBox     = "cybox"
)

// Kinds lists the primitives in dependency order: each may only refer to
// kinds before it, except that a network attaches services.
var Kinds = []string{KindGateway, KindNetwork, KindService, KindBox}

// Network is a Linux bridge: one broadcast domain on 10.42.<Index>.0/24.
// What its members reach beyond each other is exactly what it attaches.
type Network struct {
	Name     string
	Index    int      // 0 to 254; picks the /24
	Services []string // Attach=service:<name>, any number
	Gateway  string   // Attach=gateway:<name>, at most one; "" = sealed
}

// Prefix is the network's subnet.
func (n *Network) Prefix() netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 42, byte(n.Index), 0}), 24)
}

// Router is the gateway's leg (.1); Host is the host's address, which
// serves DNS and is where the proxy dials from (.2).
func (n *Network) Router() netip.Addr { return n.addr(1) }
func (n *Network) Host() netip.Addr   { return n.addr(2) }

func (n *Network) addr(last byte) netip.Addr {
	return netip.AddrFrom4([4]byte{10, 42, byte(n.Index), last})
}

// Gateway is a netns with one uplink. Networks and services plug their
// own legs into it.
type Gateway struct {
	Name   string
	Kind   string // "direct" or "wireguard"
	Config string // wireguard: path to a wg-quick style file
	DNS    string // upstream resolver for its networks; may be empty
}

// Workload is what cybox and cyservice share: an OCI image and how to run
// it.
type Workload struct {
	Image    string
	Exec     []string // overrides the image's ENTRYPOINT+CMD
	Env      []string // KEY=VALUE, added to the image's
	WorkDir  string
	User     string
	CPUs     int
	MemoryMB int
}

// Service is a runc container with a leg in every network that attaches
// it and a link to one gateway.
type Service struct {
	Name     string
	Workload Workload
	Gateway  string
	Port     int
	Secrets  []Secret
}

// Secret is handed to the service by systemd (LoadCredential=) and shows
// up as /run/secrets/<Name> in the container. cyac never reads it.
type Secret struct {
	Name string
	Path string // absolute path on the host
}

// Box is a VM (or runc container) with a disk, in one network.
type Box struct {
	Name      string
	Workload  Workload
	Network   string
	Address   netip.Addr
	Runtime   string // "" picks the first available
	DiskMB    int
	Autostart bool // started with cyac.target
}

// Config is every object in the conf dir that parsed and checked out.
type Config struct {
	Networks map[string]*Network
	Gateways map[string]*Gateway
	Services map[string]*Service
	Boxes    map[string]*Box
}

// Problem is a file that can't be used, and why. A file is also a problem
// when something it depends on is.
type Problem struct {
	Kind, Name string
	Err        error
}

func (p Problem) Error() string { return fmt.Sprintf("%s.%s: %v", p.Name, p.Kind, p.Err) }

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// ValidName reports whether name can name an object. Names end up in unit
// names, interface names and hostnames, so they stay short and plain.
func ValidName(name string) bool { return validName.MatchString(name) }

// Load reads every object file in dir. Files that fail to parse or check,
// or that depend on one that did, come back as problems and are left out
// of the Config, so one bad file never takes down unrelated objects.
func Load(dir string) (*Config, []Problem, error) {
	c := &Config{
		Networks: map[string]*Network{},
		Gateways: map[string]*Gateway{},
		Services: map[string]*Service{},
		Boxes:    map[string]*Box{},
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var probs []Problem
	for _, e := range ents {
		name, kind, ok := strings.Cut(e.Name(), ".")
		if !ok || !slices.Contains(Kinds, kind) || e.IsDir() {
			continue
		}
		if !ValidName(name) {
			probs = append(probs, Problem{kind, name, fmt.Errorf("invalid name")})
			continue
		}
		if err := c.parseFile(filepath.Join(dir, e.Name()), kind, name); err != nil {
			probs = append(probs, Problem{kind, name, err})
		}
	}
	return c, append(probs, c.check()...), nil
}

// Path is where the file for an object lives in dir.
func Path(dir, kind, name string) string { return filepath.Join(dir, name+"."+kind) }

func (c *Config) parseFile(path, kind, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	secs, err := parseINI(f)
	if err != nil {
		return err
	}
	byName := map[string]*fields{}
	for _, s := range secs {
		if byName[s.name] != nil {
			return fmt.Errorf("section [%s] given more than once", s.name)
		}
		byName[s.name] = newFields(s)
	}
	take := func(sec string) *fields {
		f := byName[sec]
		delete(byName, sec)
		if f == nil {
			f = newFields(section{name: sec})
		}
		return f
	}
	switch kind {
	case KindNetwork:
		err = c.parseNetwork(name, take("Network"))
	case KindGateway:
		err = c.parseGateway(name, take("Gateway"))
	case KindService:
		err = c.parseService(name, take("Workload"), take("Service"))
	case KindBox:
		err = c.parseBox(name, take("Workload"), take("Box"))
	}
	if err != nil {
		return err
	}
	for sec := range byName {
		return fmt.Errorf("unknown section [%s]", sec)
	}
	return nil
}

func (c *Config) parseNetwork(name string, f *fields) error {
	n := &Network{Name: name}
	idx, err := f.one("Index")
	if err != nil {
		return err
	}
	if n.Index, err = intIn(idx, "Index", 0, 254, true); err != nil {
		return err
	}
	for _, a := range f.all("Attach") {
		kind, target, _ := strings.Cut(a, ":")
		switch {
		case !ValidName(target):
			return bad("Attach", a, "want service:<name> or gateway:<name>")
		case kind == "service":
			if slices.Contains(n.Services, target) {
				return bad("Attach", a, "given twice")
			}
			n.Services = append(n.Services, target)
		case kind == "gateway":
			if n.Gateway != "" {
				return fmt.Errorf("a network attaches at most one gateway")
			}
			n.Gateway = target
		default:
			return bad("Attach", a, "want service:<name> or gateway:<name>")
		}
	}
	if err := f.done(); err != nil {
		return err
	}
	c.Networks[name] = n
	return nil
}

func (c *Config) parseGateway(name string, f *fields) error {
	g := &Gateway{Name: name}
	var err error
	for _, kv := range []struct {
		key string
		dst *string
	}{{"Kind", &g.Kind}, {"Config", &g.Config}, {"DNS", &g.DNS}} {
		if *kv.dst, err = f.one(kv.key); err != nil {
			return err
		}
	}
	if err := f.done(); err != nil {
		return err
	}
	switch g.Kind {
	case "direct":
		if g.Config != "" {
			return bad("Config", g.Config, "only applies to Kind=wireguard")
		}
	case "wireguard":
		if !filepath.IsAbs(g.Config) {
			return bad("Config", g.Config, "Kind=wireguard needs an absolute path")
		}
	default:
		return bad("Kind", g.Kind, "want direct or wireguard")
	}
	if g.DNS != "" {
		if _, err := netip.ParseAddr(g.DNS); err != nil {
			return bad("DNS", g.DNS, err.Error())
		}
	}
	c.Gateways[name] = g
	return nil
}

var secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func (c *Config) parseService(name string, wf, f *fields) error {
	s := &Service{Name: name}
	var err error
	if s.Workload, err = parseWorkload(wf); err != nil {
		return err
	}
	if s.Gateway, err = f.one("Gateway"); err != nil {
		return err
	}
	if !ValidName(s.Gateway) {
		return fmt.Errorf("[Service] needs Gateway=<name>")
	}
	port, err := f.one("Port")
	if err != nil {
		return err
	}
	if s.Port, err = intIn(port, "Port", 1, 65535, true); err != nil {
		return err
	}
	for _, v := range f.all("Secret") {
		n, p, _ := strings.Cut(v, ":")
		if !secretName.MatchString(n) || !filepath.IsAbs(p) {
			return bad("Secret", v, "want NAME:/absolute/path")
		}
		if slices.ContainsFunc(s.Secrets, func(x Secret) bool { return x.Name == n }) {
			return bad("Secret", v, "name given twice")
		}
		s.Secrets = append(s.Secrets, Secret{n, p})
	}
	if err := f.done(); err != nil {
		return err
	}
	c.Services[name] = s
	return nil
}

func (c *Config) parseBox(name string, wf, f *fields) error {
	b := &Box{Name: name}
	var err error
	if b.Workload, err = parseWorkload(wf); err != nil {
		return err
	}
	if b.Network, err = f.one("Network"); err != nil {
		return err
	}
	if !ValidName(b.Network) {
		return fmt.Errorf("[Box] needs Network=<name>")
	}
	addr, err := f.one("Address")
	if err != nil {
		return err
	}
	if b.Address, err = netip.ParseAddr(addr); err != nil || !b.Address.Is4() {
		return fmt.Errorf("[Box] needs Address= with an IPv4 address, got %q", addr)
	}
	if b.Runtime, err = f.one("Runtime"); err != nil {
		return err
	}
	disk, err := f.one("DiskSize")
	if err != nil {
		return err
	}
	if b.DiskMB, err = sizeMB(disk, "DiskSize"); err != nil {
		return err
	}
	auto, err := f.one("Autostart")
	if err != nil {
		return err
	}
	if b.Autostart, err = boolean(auto, "Autostart"); err != nil {
		return err
	}
	if err := f.done(); err != nil {
		return err
	}
	c.Boxes[name] = b
	return nil
}

func parseWorkload(f *fields) (Workload, error) {
	var w Workload
	var err error
	if w.Image, err = f.one("Image"); err != nil {
		return w, err
	}
	if w.Image == "" {
		return w, fmt.Errorf("[Workload] needs Image=")
	}
	exec, err := f.one("Exec")
	if err != nil {
		return w, err
	}
	if w.Exec, err = splitWords(exec); err != nil {
		return w, bad("Exec", exec, err.Error())
	}
	w.Env = f.all("Env")
	for _, kv := range w.Env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return w, bad("Env", kv, "want KEY=VALUE")
		}
	}
	if w.WorkDir, err = f.one("WorkingDirectory"); err != nil {
		return w, err
	}
	if w.User, err = f.one("User"); err != nil {
		return w, err
	}
	cpus, err := f.one("CPUs")
	if err != nil {
		return w, err
	}
	if w.CPUs, err = intIn(cpus, "CPUs", 1, 256, false); err != nil {
		return w, err
	}
	mem, err := f.one("Memory")
	if err != nil {
		return w, err
	}
	if w.MemoryMB, err = sizeMB(mem, "Memory"); err != nil {
		return w, err
	}
	return w, f.done()
}

// check resolves references between objects. It drops every object that
// refers to a missing or dropped one, until nothing changes, and reports
// each drop.
func (c *Config) check() []Problem {
	var probs []Problem
	drop := func(kind, name string, err error) {
		probs = append(probs, Problem{kind, name, err})
		switch kind {
		case KindNetwork:
			delete(c.Networks, name)
		case KindService:
			delete(c.Services, name)
		case KindBox:
			delete(c.Boxes, name)
		}
	}

	// Checks between objects of the same kind first; they don't cascade.
	byIndex := map[int]string{}
	for _, name := range Sorted(c.Networks) {
		n := c.Networks[name]
		if other, ok := byIndex[n.Index]; ok {
			drop(KindNetwork, name, bad("Index", strconv.Itoa(n.Index), "already used by network "+other))
			continue
		}
		byIndex[n.Index] = name
	}

	for changed := true; changed; {
		changed = false
		for _, name := range Sorted(c.Networks) {
			n := c.Networks[name]
			if n.Gateway != "" && c.Gateways[n.Gateway] == nil {
				drop(KindNetwork, name, fmt.Errorf("gateway %s: not usable", n.Gateway))
				changed = true
			}
		}
		for _, name := range Sorted(c.Services) {
			s := c.Services[name]
			if c.Gateways[s.Gateway] == nil {
				drop(KindService, name, fmt.Errorf("gateway %s: not usable", s.Gateway))
				changed = true
			}
		}
		for _, name := range Sorted(c.Networks) {
			for _, s := range c.Networks[name].Services {
				if c.Services[s] == nil {
					drop(KindNetwork, name, fmt.Errorf("service %s: not usable", s))
					changed = true
					break
				}
			}
		}
		for _, name := range Sorted(c.Boxes) {
			if c.Networks[c.Boxes[name].Network] == nil {
				drop(KindBox, name, fmt.Errorf("network %s: not usable", c.Boxes[name].Network))
				changed = true
			}
		}
	}

	// Addresses: inside the network's /24, out of the reserved .0 to .9
	// and .255, and unique.
	used := map[netip.Addr]string{}
	for _, name := range Sorted(c.Boxes) {
		b := c.Boxes[name]
		n := c.Networks[b.Network]
		last := b.Address.As4()[3]
		switch {
		case !n.Prefix().Contains(b.Address):
			drop(KindBox, name, bad("Address", b.Address.String(), fmt.Sprintf("outside network %s (%s)", n.Name, n.Prefix())))
		case last < 10 || last == 255:
			drop(KindBox, name, bad("Address", b.Address.String(), ".0 to .9 and .255 are reserved"))
		case used[b.Address] != "":
			drop(KindBox, name, bad("Address", b.Address.String(), "already used by box "+used[b.Address]))
		default:
			used[b.Address] = name
		}
	}
	return probs
}

// AttachedBy lists the networks that attach service s, sorted.
func (c *Config) AttachedBy(s string) []string {
	var out []string
	for _, name := range Sorted(c.Networks) {
		if slices.Contains(c.Networks[name].Services, s) {
			out = append(out, name)
		}
	}
	return out
}

// Sorted returns m's keys in order, for callers that need stable output.
func Sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// bad is the error for a key whose value can't be used.
func bad(key, value, why string) error { return fmt.Errorf("%s=%s: %s", key, value, why) }

func intIn(s, key string, lo, hi int, required bool) (int, error) {
	if s == "" {
		if required {
			return 0, fmt.Errorf("%s= is required", key)
		}
		return 0, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%s=%s: want a number from %d to %d", key, s, lo, hi)
	}
	return v, nil
}

// sizeMB parses a size with an M or G suffix (a bare number is MiB).
func sizeMB(s, key string) (int, error) {
	if s == "" {
		return 0, nil
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1024, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		s = strings.TrimSuffix(s, "M")
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 || v > 1<<30/mult {
		return 0, fmt.Errorf("%s=%s: want a size like 512M or 4G", key, s)
	}
	return v * mult, nil
}

func boolean(s, key string) (bool, error) {
	switch s {
	case "", "no", "false", "0":
		return false, nil
	case "yes", "true", "1":
		return true, nil
	}
	return false, fmt.Errorf("%s=%s: want yes or no", key, s)
}

// splitWords splits a command line on spaces, honouring single and double
// quotes and backslash escapes inside double quotes. No variables, no
// globbing.
func splitWords(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		in    bool // inside a word
		quote rune
	)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == 0 && (r == ' ' || r == '\t'):
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		case quote == 0 && (r == '"' || r == '\''):
			quote, in = r, true
		case quote != 0 && r == quote:
			quote = 0
		case quote == '"' && r == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}
