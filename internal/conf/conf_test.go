package conf

import (
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
	out = "[Gateway]\nKind=direct\n"
	dev = "[Network]\nAttach=service:proxy\nAttach=gateway:out\n"
	prx = "[Service]\nImage=example/proxy\nExec=/bin/proxy --listen ':8080' \"a b\"\nEnv=A=1\nEnv=B=2\n" +
		"DiskSize=1G\nRuntime=runc\nGateway=out\nPort=8080\nSecret=KEY:/run/keys/key\n"
	bx = "[Box]\nImage=example/box\nCPU=2\nMemory=4G\nNetwork=dev\nDiskSize=10G\nAutostart=yes\n"
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
		Networks: map[string]*Network{"dev": {Name: "dev", Services: []string{"proxy"}, Gateway: "out"}},
		Services: map[string]*Service{"proxy": {
			Name: "proxy",
			Workload: Workload{
				Image:  "example/proxy",
				Exec:   []string{"/bin/proxy", "--listen", ":8080", "a b"},
				Env:    []string{"A=1", "B=2"},
				DiskMB: 1024, Runtime: "runc",
			},
			Gateway: "out", Port: 8080,
			Secrets: []Secret{{"KEY", "/run/keys/key"}},
		}},
		Boxes: map[string]*Box{"a": {
			Name:      "a",
			Workload:  Workload{Image: "example/box", CPUs: 2, MemoryMB: 4096, DiskMB: 10240},
			Network:   "dev",
			Autostart: true,
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
		{"x.cynetwork", "[Network]\nAttach=gateway:out\nAttach=gateway:out\n", "at most one gateway"},
		{"x.cynetwork", "[Network]\nAttach=box:a\n", "want service:<name>"},
		{"x.cynetwork", "[Network]\nIsolated=yes\n", "unknown key Isolated="},
		{"x.cynetwork", "[Netwrok]\n", "unknown section [Netwrok]"},
		{"x.cynetwork", "Attach=gateway:out\n", "outside a section"},
		{"x.cynetwork", "[Network]\nIndex=3\n", "unknown key Index="},
		{"x.cybox", "[Box]\nImage=i\nNetwork=dev\nAddress=10.42.1.10\n", "unknown key Address="},
		{"x.cygateway", "[Gateway]\nKind=tor\n", "want direct or wireguard"},
		{"x.cygateway", "[Gateway]\nKind=wireguard\n", "Kind=wireguard needs an absolute path"},
		{"x.cygateway", "[Gateway]\nKind=wireguard\nConfig=/k\n", "needs the tunnel's IPv4 address"},
		{"x.cygateway", "[Gateway]\nKind=direct\nAddress=10.0.0.1/32\n", "only apply to Kind=wireguard"},
		{"x.cygateway", "[Gateway]\nKind=direct\nDNS=resolver\n", "DNS=resolver"},
		{"x.cygateway", "[Gateway]\nKind=direct\nUplink=eth0\"\n", "want a host interface name"},
		{"x.cygateway", "[Gateway]\nKind=direct\nUplink=averyverylongname\n", "want a host interface name"},
		{"x.cygateway", "[Gateway]\nKind=wireguard\nConfig=/k\nAddress=10.64.0.2/32\nUplink=eth0\n", "only applies to Kind=direct"},
		{"x.cyservice", "[Service]\nImage=i\nGateway=out\nPort=0\n", "Port=0"},
		{"x.cyservice", "[Service]\nImage=i\nGateway=out\nPort=1\nSecret=KEY:rel\n", "NAME:/absolute/path"},
		{"x.cyservice", "[Service]\nImage=i\nExec=sh -c 'oops\nGateway=out\nPort=1\n", "unterminated"},
		{"x.cyservice", "[Workload]\nImage=i\n[Service]\nGateway=out\nPort=1\n", "unknown section [Workload]"},
		{"x.cybox", "[Box]\nImage=i\nMemory=lots\nNetwork=dev\n", "Memory=lots"},
		{"x.cybox", "[Box]\nImage=i\nNetwork=dev\nAutostart=maybe\n", "want yes or no"},
		{"x.cybox", "[Box]\nImage=i\nNetwork=dev\n[Extra]\n", "unknown section [Extra]"},
		{"x.cybox", "[Workload]\nImage=i\n[Box]\nNetwork=dev\n", "unknown section [Workload]"},
		{"x.cybox", "[Box]\nImage=i\nNetwork=dev\nRoot=maybe\n", "want keep or replace"},
		{"x.cybox", "[Box]\nImage=i\nNetwork=dev\nVolume=home\n", "clean absolute path"},
		{"x.cybox", "[Box]\nImage=i\nNetwork=dev\nVolume=/\n", "clean absolute path"},
		{"x.cybox", "[Box]\nCPUs=2\nNetwork=dev\n", "unknown key CPUs="},
		{"Bad.cybox", "", "invalid name"},
		{"proxy.cybox", bx, "name already used by cyservice proxy"},
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
		"web.cynetwork":   "[Network]\nAttach=gateway:out\n",
		"b.cybox":         strings.Replace(bx, "dev", "web", 1),
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

// A file rejected for a stray section leaves nothing behind in the Config.
func TestRejectedFileLeftOut(t *testing.T) {
	c, errs := load(t, map[string]string{
		"out.cygateway":   out,
		"proxy.cyservice": "[Workload]\nImage=i\n[Service]\nGateway=out\nPort=1\n",
	})
	if errs["proxy.cyservice"] == "" || c.Services["proxy"] != nil {
		t.Errorf("problems %v, service %+v", errs, c.Services["proxy"])
	}
}
