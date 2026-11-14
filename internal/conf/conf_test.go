package conf

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// write puts files (name to content) into a fresh conf dir.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func load(t *testing.T, files map[string]string) (*Config, map[string]string) {
	t.Helper()
	c, probs, err := Load(write(t, files))
	if err != nil {
		t.Fatal(err)
	}
	errs := map[string]string{}
	for _, p := range probs {
		errs[p.Name+"."+p.Kind] = p.Err.Error()
	}
	return c, errs
}

const (
	out = "[Gateway]\nIndex=0\nKind=direct\n"
	dev = "[Network]\nIndex=1\nAttach=service:proxy\nAttach=gateway:out\n"
	prx = "[Workload]\nImage=example/proxy\nExec=/bin/proxy --listen ':8080' \"a b\"\nEnv=A=1\nEnv=B=2\n\n" +
		"[Service]\nIndex=0\nGateway=out\nPort=8080\nSecret=KEY:/run/keys/key\n"
	bx = "[Workload]\nImage=example/box\nCPUs=2\nMemory=4G\n\n[Box]\nNetwork=dev\nAddress=10.42.1.10\nDiskSize=10G\nAutostart=yes\n"
)

func TestLoad(t *testing.T) {
	c, errs := load(t, map[string]string{
		"out.cygateway":   out,
		"dev.cynetwork":   dev,
		"proxy.cyservice": prx,
		"a.cybox":         bx,
		"README":          "not an object",
		"notes.txt":       "neither",
	})
	if len(errs) != 0 {
		t.Fatalf("problems: %v", errs)
	}
	want := &Config{
		Gateways: map[string]*Gateway{"out": {Name: "out", Kind: "direct"}},
		Networks: map[string]*Network{"dev": {Name: "dev", Index: 1, Services: []string{"proxy"}, Gateway: "out"}},
		Services: map[string]*Service{"proxy": {
			Name: "proxy",
			Workload: Workload{
				Image: "example/proxy",
				Exec:  []string{"/bin/proxy", "--listen", ":8080", "a b"},
				Env:   []string{"A=1", "B=2"},
			},
			Gateway: "out", Port: 8080,
			Secrets: []Secret{{"KEY", "/run/keys/key"}},
		}},
		Boxes: map[string]*Box{"a": {
			Name:     "a",
			Workload: Workload{Image: "example/box", CPUs: 2, MemoryMB: 4096},
			Network:  "dev", Address: netip.MustParseAddr("10.42.1.10"),
			DiskMB: 10240, Autostart: true,
		}},
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("got  %+v\nwant %+v", c, want)
	}
	if got := c.AttachedBy("proxy"); !slices.Equal(got, []string{"dev"}) {
		t.Errorf("AttachedBy = %v", got)
	}
}

func TestLoadMissingDir(t *testing.T) {
	c, probs, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(probs) != 0 || len(c.Boxes) != 0 {
		t.Fatalf("got %v, %v, %v", c, probs, err)
	}
}

// Each case is one bad file next to good ones; it must be the only
// problem, and its error must say what is wrong.
func TestFileErrors(t *testing.T) {
	for _, tc := range []struct {
		file, content, want string
	}{
		{"x.cynetwork", "[Network]\n", "Index= is required"},
		{"x.cynetwork", "[Network]\nIndex=254\n", "from 0 to 253"},
		{"x.cynetwork", "[Network]\nIndex=3\nAttach=gateway:out\nAttach=gateway:out\n", "at most one gateway"},
		{"x.cynetwork", "[Network]\nIndex=3\nAttach=box:a\n", "want service:<name>"},
		{"x.cynetwork", "[Network]\nIndex=3\nIndex=4\n", "more than once"},
		{"x.cynetwork", "[Network]\nIndex=3\nIsolated=yes\n", "unknown key Isolated="},
		{"x.cynetwork", "[Netwrok]\nIndex=3\n", "Index= is required"},
		{"x.cynetwork", "Index=3\n", "outside a section"},
		{"x.cygateway", "[Gateway]\nIndex=5\nKind=tor\n", "want direct or wireguard"},
		{"x.cygateway", "[Gateway]\nIndex=5\nKind=wireguard\n", "Kind=wireguard needs an absolute path"},
		{"x.cygateway", "[Gateway]\nIndex=5\nKind=wireguard\nConfig=/k\n", "needs the tunnel's IPv4 address"},
		{"x.cygateway", "[Gateway]\nIndex=5\nKind=direct\nAddress=10.0.0.1/32\n", "only apply to Kind=wireguard"},
		{"x.cygateway", "[Gateway]\nIndex=5\nKind=direct\nDNS=resolver\n", "DNS=resolver"},
		{"x.cygateway", "[Gateway]\nIndex=5\nKind=direct\nUplink=eth0\"\n", "want a host interface name"},
		{"x.cygateway", "[Gateway]\nIndex=5\nKind=direct\nUplink=averyverylongname\n", "want a host interface name"},
		{"x.cygateway", "[Gateway]\nIndex=5\nKind=wireguard\nConfig=/k\nAddress=10.64.0.2/32\nUplink=eth0\n", "only applies to Kind=direct"},
		{"x.cyservice", "[Service]\nIndex=5\nGateway=out\nPort=80\n", "needs Image="},
		{"x.cyservice", "[Workload]\nImage=i\n[Service]\nIndex=5\nGateway=out\nPort=0\n", "Port=0"},
		{"x.cyservice", "[Workload]\nImage=i\n[Service]\nIndex=5\nGateway=out\nPort=1\nSecret=KEY:rel\n", "NAME:/absolute/path"},
		{"x.cyservice", "[Workload]\nImage=i\nExec=sh -c 'oops\n[Service]\nIndex=5\nGateway=out\nPort=1\n", "unterminated"},
		{"x.cybox", "[Workload]\nImage=i\n[Box]\nNetwork=dev\n", "needs Address="},
		{"x.cybox", "[Workload]\nImage=i\nMemory=lots\n[Box]\nNetwork=dev\nAddress=10.42.1.11\n", "Memory=lots"},
		{"x.cybox", "[Workload]\nImage=i\n[Box]\nNetwork=dev\nAddress=10.42.1.11\nAutostart=maybe\n", "want yes or no"},
		{"x.cybox", "[Workload]\nImage=i\n[Box]\nNetwork=dev\nAddress=10.42.1.11\n[Extra]\n", "unknown section [Extra]"},
		{"Bad.cybox", "", "invalid name"},
		{"x.cygateway", "[Gateway]\nIndex=0\nKind=direct\n", "Index=0: already used by cygateway out"},
		{"x.cyservice", strings.Replace(prx, "Index=0", "Index=128", 1), "from 0 to 127"},
		{"proxy.cybox", strings.Replace(bx, "10.42.1.10", "10.42.1.11", 1), "name already used by cyservice proxy"},
	} {
		t.Run(tc.file+"/"+tc.want, func(t *testing.T) {
			_, errs := load(t, map[string]string{
				"out.cygateway": out, "dev.cynetwork": dev, "proxy.cyservice": prx, "a.cybox": bx,
				tc.file: tc.content,
			})
			key := strings.TrimSuffix(tc.file, filepath.Ext(tc.file)) + filepath.Ext(tc.file)
			if len(errs) != 1 || !strings.Contains(errs[key], tc.want) {
				t.Errorf("problems %v, want only %s containing %q", errs, key, tc.want)
			}
		})
	}
}

// A missing gateway takes down the service that uses it, the network that
// attaches the service, and the box on the network. Unrelated objects stay.
func TestCascade(t *testing.T) {
	c, errs := load(t, map[string]string{
		"dev.cynetwork":   dev,
		"proxy.cyservice": strings.Replace(prx, "Gateway=out", "Gateway=gone", 1),
		"a.cybox":         bx,
		"out.cygateway":   out,
		"web.cynetwork":   "[Network]\nIndex=2\nAttach=gateway:out\n",
		"b.cybox":         strings.NewReplacer("dev", "web", "10.42.1.10", "10.42.2.10").Replace(bx),
	})
	for _, key := range []string{"proxy.cyservice", "dev.cynetwork", "a.cybox"} {
		if errs[key] == "" {
			t.Errorf("%s: no problem reported", key)
		}
	}
	if len(errs) != 3 {
		t.Errorf("problems: %v", errs)
	}
	if c.Networks["web"] == nil || c.Boxes["b"] == nil || c.Gateways["out"] == nil {
		t.Errorf("unrelated objects dropped: %+v", c)
	}
}

func TestAddresses(t *testing.T) {
	box := func(addr string) string { return strings.Replace(bx, "10.42.1.10", addr, 1) }
	_, errs := load(t, map[string]string{
		"out.cygateway": out, "dev.cynetwork": dev, "proxy.cyservice": prx,
		"a.cybox":     bx,
		"b.cybox":     box("10.42.1.10"), // taken by a
		"c.cybox":     box("10.42.2.10"), // other network
		"d.cybox":     box("10.42.1.2"),  // reserved
		"e.cybox":     box("10.42.1.11"),
		"f.cybox":     box("10.42.1.254"),     // proxy's address (Index 0)
		"x.cynetwork": "[Network]\nIndex=1\n", // index taken by dev
	})
	want := map[string]string{
		"b.cybox":     "already used by box a",
		"c.cybox":     "outside network dev",
		"d.cybox":     "reserved",
		"f.cybox":     "already used by service proxy",
		"x.cynetwork": "Index=1: already used by cynetwork dev",
	}
	if len(errs) != len(want) {
		t.Errorf("problems: %v", errs)
	}
	for k, v := range want {
		if !strings.Contains(errs[k], v) {
			t.Errorf("%s: %q, want %q", k, errs[k], v)
		}
	}
}

func TestSplitWords(t *testing.T) {
	for in, want := range map[string][]string{
		"":                     nil,
		"  a  b ":              {"a", "b"},
		`sh -c 'echo "hi"; x'`: {"sh", "-c", `echo "hi"; x`},
		`a "b \" c" ''`:        {"a", `b " c`, ""},
		`--flag="x y"`:         {"--flag=x y"},
	} {
		got, err := splitWords(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitWords(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestINIContinuation(t *testing.T) {
	secs, err := parseINI(strings.NewReader("; comment\n[A]\nExec=one \\\n  two\n# c\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) != 1 || len(secs[0].entries) != 1 || secs[0].entries[0].value != "one two" {
		t.Fatalf("got %+v", secs)
	}
}
