package run

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/containmentzone/cyacrt/internal/driver"
)

// fakeRuntime records the machine it started; the workload exits at once.
type fakeRuntime struct {
	name      string
	kind      driver.LinkKind
	missing   bool
	started   *driver.Machine
	cleanedUp []string
}

func (f *fakeRuntime) Name() string { return f.name }
func (f *fakeRuntime) Available() error {
	if f.missing {
		return errors.New("not here")
	}
	return nil
}
func (f *fakeRuntime) LinkKind() driver.LinkKind                            { return f.kind }
func (f *fakeRuntime) Start(m *driver.Machine) error                        { f.started = m; return nil }
func (f *fakeRuntime) Stop(*driver.Machine) error                           { return nil }
func (f *fakeRuntime) Running(*driver.Machine) bool                         { return false }
func (f *fakeRuntime) Cleanup(m *driver.Machine)                            { f.cleanedUp = append(f.cleanedUp, m.RunDir) }
func (f *fakeRuntime) Grow(*driver.Machine) error                           { return nil }
func (f *fakeRuntime) Command(*driver.Machine, []string) (*exec.Cmd, error) { return nil, nil }

// seeded pretends name's disk was seeded from an image with these defaults.
func seeded(t *testing.T, dir, name, meta string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "disks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disks", name+".json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

const sealed = "[Network]\nIndex=3\n"

func TestBox(t *testing.T) {
	r, f := runner(t, map[string]string{
		"lab.cynetwork": sealed,
		"a.cybox": "[Workload]\nImage=img\nExec=/bin/app --x\nEnv=B=2\nMemory=2G\n\n" +
			"[Box]\nNetwork=lab\nAddress=10.42.3.10\n",
	})
	vm := &fakeRuntime{name: "vm", kind: driver.Tap}
	r.State, r.Runtimes = t.TempDir(), []driver.Runtime{&fakeRuntime{name: "gone", missing: true}, vm}
	seeded(t, r.State, "a", `{"init":["/sbin/init"],"env":["A=1"],"run_as":"app","cwd":"/srv"}`)

	if err := r.Start(context.Background(), "cybox", "a"); err != nil {
		t.Fatal(err)
	}
	m := vm.started
	if m == nil {
		t.Fatal("not started")
	}
	if m.Link.HostIface != "cyb3-10" || m.Link.Netns != "" || m.MAC != "02:c7:ac:00:03:0a" {
		t.Errorf("link %+v, mac %s", m.Link, m.MAC)
	}
	if m.Addr.Gateway.IsValid() || m.Addr.DNS != "10.42.3.2" {
		t.Errorf("addr %+v: a sealed network has no gateway", m.Addr)
	}
	if !slices.Equal(m.Init.Args, []string{"/bin/app", "--x"}) || m.Init.Mode != "command" ||
		!slices.Equal(m.Init.Env, []string{"A=1", "B=2"}) || m.Init.User != "app" || m.Init.Cwd != "/srv" {
		t.Errorf("init %+v", m.Init)
	}
	if m.CPUs != 1 || m.MemMiB != 2048 {
		t.Errorf("cpus %d, mem %d", m.CPUs, m.MemMiB)
	}
	if f.ran[0] != "ip tuntap add dev cyb3-10 mode tap" {
		t.Errorf("plan ran %q", f.ran)
	}

	if err := r.Stop("cybox", "a"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(vm.cleanedUp, m.RunDir) {
		t.Errorf("cleanup missed %s: %v", m.RunDir, vm.cleanedUp)
	}
	if _, err := os.Stat(m.RunDir); !os.IsNotExist(err) {
		t.Errorf("run dir left: %v", err)
	}
}

func TestService(t *testing.T) {
	r, _ := runner(t, map[string]string{
		"out.cygateway":   "[Gateway]\nIndex=0\nKind=direct\nDNS=9.9.9.9\n",
		"dev.cynetwork":   "[Network]\nIndex=1\nAttach=service:proxy\n",
		"proxy.cyservice": "[Workload]\nImage=img\n\n[Service]\nIndex=0\nGateway=out\nPort=8080\nSecret=KEY:/k\n",
	})
	rc := &fakeRuntime{name: "runc", kind: driver.Netns}
	r.State, r.Runtimes = t.TempDir(), []driver.Runtime{&fakeRuntime{name: "vm"}, rc}
	seeded(t, filepath.Join(r.State, "services"), "proxy", `{"init":["/proxy"],"mode":"command"}`)

	if err := r.Start(context.Background(), "cyservice", "proxy"); err == nil {
		t.Error("started without $CREDENTIALS_DIRECTORY")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/credentials/cyservice@proxy.service")
	if err := r.Start(context.Background(), "cyservice", "proxy"); err != nil {
		t.Fatal(err)
	}
	m := rc.started
	want := driver.Link{Netns: "/run/netns/cysvc-proxy"}
	if !reflect.DeepEqual(m.Link, want) || m.Secrets != "/run/credentials/cyservice@proxy.service" || m.Init.DNS != "9.9.9.9" {
		t.Errorf("link %+v, secrets %q, dns %q", m.Link, m.Secrets, m.Init.DNS)
	}
	if filepath.Dir(filepath.Dir(m.Disk)) != filepath.Join(r.State, "services") {
		t.Errorf("service disk %s is outside the services tree", m.Disk)
	}
}

func TestRuntimeChoice(t *testing.T) {
	r := &Runner{Runtimes: []driver.Runtime{&fakeRuntime{name: "vm", missing: true}, &fakeRuntime{name: "runc"}}}
	if rt, err := r.runtime(""); err != nil || rt.Name() != "runc" {
		t.Errorf("first available: %v, %v", rt, err)
	}
	if _, err := r.runtime("vm"); err == nil {
		t.Error("picked an unavailable runtime")
	}
	t.Setenv("CYAC_RUNTIME", "nope")
	if _, err := r.runtime(""); err == nil {
		t.Error("ignored CYAC_RUNTIME")
	}
}
