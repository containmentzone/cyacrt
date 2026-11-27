package run

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

// fakeZFS puts a zfs on PATH that logs its arguments and says every zvol
// is 1 GiB, or, without zvols, that there are none, and returns the log.
func fakeZFS(t *testing.T, zvols bool) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	get := "echo 1073741824"
	if !zvols {
		get = "exit 1"
	}
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n[ \"$1\" = get ] && " + get + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "zfs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return log
}

const sealed = "[Network]\n"

func TestBox(t *testing.T) {
	r, f := runner(t, map[string]string{
		"lab.cynetwork": sealed,
		"a.cybox": "[Box]\nSeedImage=img\nExec=/bin/app --x\nEnv=B=2\nMemory=2G\n" +
			"Network=lab\n",
	})
	vm := &fakeRuntime{name: "cloud-hypervisor", kind: driver.Tap}
	r.State, r.Dataset, r.Runtimes = t.TempDir(), "tank/cyac", []driver.Runtime{vm, &fakeRuntime{name: "runc"}}
	zfs := fakeZFS(t, true)
	seeded(t, r.State, "a", `{"init":["/sbin/init"],"env":["A=1"],"run_as":"app","cwd":"/srv"}`)

	if err := r.Start(context.Background(), "cybox", "a"); err != nil {
		t.Fatal(err)
	}
	m := vm.started
	if m == nil {
		t.Fatal("not started")
	}
	if m.Link.HostIface != "cyb0-10" || m.Link.Netns != "" || m.MAC != "02:c7:ac:00:00:0a" {
		t.Errorf("link %+v, mac %s", m.Link, m.MAC)
	}
	if m.Addr.Gateway.IsValid() || m.Init.DNS != "" {
		t.Errorf("addr %+v: a sealed network has no gateway and no resolver", m.Addr)
	}
	if !slices.Equal(m.Init.Args, []string{"/bin/app", "--x"}) || m.Init.Mode != "command" ||
		!slices.Equal(m.Init.Env, []string{"A=1", "B=2"}) || m.Init.User != "app" || m.Init.Cwd != "/srv" {
		t.Errorf("init %+v", m.Init)
	}
	if m.CPUs != 1 || m.MemMiB != 2048 {
		t.Errorf("cpus %d, mem %d", m.CPUs, m.MemMiB)
	}
	// The 1 GiB zvol grows to the default 10 GiB DiskSize.
	if m.Disk != "/dev/zvol/tank/cyac/boxes/a" {
		t.Errorf("disk %s", m.Disk)
	}
	if calls, _ := os.ReadFile(zfs); !strings.Contains(string(calls), "set volsize=10737418240 tank/cyac/boxes/a\n") {
		t.Errorf("zvol not grown: %s", calls)
	}
	if f.ran[0] != "ip tuntap add dev cyb0-10 mode tap" {
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

// A box without SeedImage= starts from the disk it has, and can't start
// without one.
func TestBoxNoSeedImage(t *testing.T) {
	r, _ := runner(t, map[string]string{
		"lab.cynetwork": sealed,
		"a.cybox":       "[Box]\nNetwork=lab\n",
	})
	vm := &fakeRuntime{name: "cloud-hypervisor", kind: driver.Tap}
	r.State, r.Dataset, r.Runtimes = t.TempDir(), "tank/cyac", []driver.Runtime{vm}
	fakeZFS(t, false)
	if err := r.Start(context.Background(), "cybox", "a"); err == nil || !strings.Contains(err.Error(), "SeedImage=") {
		t.Fatalf("got %v, want a missing SeedImage= error", err)
	}
	seeded(t, r.State, "a", `{"init":["/sbin/init"]}`)
	fakeZFS(t, true)
	if err := r.Start(context.Background(), "cybox", "a"); err != nil {
		t.Fatal(err)
	}
}

func TestService(t *testing.T) {
	r, _ := runner(t, map[string]string{
		"out.cygateway":   "[Gateway]\nKind=direct\nDNS=9.9.9.9\n",
		"dev.cynetwork":   "[Network]\nAttach=service:proxy\n",
		"proxy.cyservice": "[Service]\nSeedImage=img\nRuntime=runc\nGateway=out\nPort=8080\nSecret=KEY:/k\n",
	})
	rc := &fakeRuntime{name: "runc", kind: driver.Netns}
	r.State, r.Dataset, r.Runtimes = t.TempDir(), "tank/cyac", []driver.Runtime{&fakeRuntime{name: "vm"}, rc}
	fakeZFS(t, true)
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
	if !reflect.DeepEqual(m.Link, want) || m.Secrets != "/run/credentials/cyservice@proxy.service" || m.Init.DNS != "10.42.255.0" { // its gateway end
		t.Errorf("link %+v, secrets %q, dns %q", m.Link, m.Secrets, m.Init.DNS)
	}
	if m.Disk != "/dev/zvol/tank/cyac/services/proxy" {
		t.Errorf("service disk %s is outside the services dataset", m.Disk)
	}
}

// A box whose zvol exists without its saved image defaults is left alone,
// never destroyed and reseeded.
func TestBoxKeepsUnknownDisk(t *testing.T) {
	r, _ := runner(t, map[string]string{
		"lab.cynetwork": sealed,
		"a.cybox":       "[Box]\nSeedImage=img\nNetwork=lab\n",
	})
	vm := &fakeRuntime{name: "cloud-hypervisor", kind: driver.Tap}
	r.State, r.Dataset, r.Runtimes = t.TempDir(), "tank/cyac", []driver.Runtime{vm}
	zfs := fakeZFS(t, true)
	if err := r.Start(context.Background(), "cybox", "a"); err == nil || !strings.Contains(err.Error(), "exists but") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if calls, _ := os.ReadFile(zfs); strings.Contains(string(calls), "destroy") || strings.Contains(string(calls), "create") {
		t.Errorf("touched the zvol: %s", calls)
	}
}

// A box runs under a VM unless it asks for another runtime: no KVM means
// it doesn't start, not that it lands in runc.
func TestBoxRuntime(t *testing.T) {
	conf := map[string]string{
		"lab.cynetwork": sealed,
		"a.cybox":       "[Box]\nNetwork=lab\n",
		"b.cybox":       "[Box]\nNetwork=lab\nRuntime=runc\n",
	}
	r, _ := runner(t, conf)
	vm := &fakeRuntime{name: "cloud-hypervisor", kind: driver.Tap, missing: true}
	rc := &fakeRuntime{name: "runc", kind: driver.Netns}
	r.State, r.Dataset, r.Runtimes = t.TempDir(), "tank/cyac", []driver.Runtime{vm, rc}
	fakeZFS(t, true)
	seeded(t, r.State, "a", `{"init":["/sbin/init"]}`)
	seeded(t, r.State, "b", `{"init":["/sbin/init"]}`)

	if err := r.Start(context.Background(), "cybox", "a"); err == nil || !strings.Contains(err.Error(), "Runtime=runc") {
		t.Errorf("got %v, want a refusal that names Runtime=runc", err)
	}
	if rc.started != nil {
		t.Fatal("fell back to runc")
	}
	if err := r.Start(context.Background(), "cybox", "b"); err != nil || rc.started == nil {
		t.Errorf("Runtime=runc: %v", err)
	}
	t.Setenv("CYAC_RUNTIME", "nope")
	if err := r.Start(context.Background(), "cybox", "a"); err == nil || !strings.Contains(err.Error(), `no runtime "nope"`) {
		t.Errorf("ignored CYAC_RUNTIME: %v", err)
	}
}

// A service runs under gVisor unless its file names runc, never falls
// back to runc on its own, and never runs in a VM.
func TestServiceRuntime(t *testing.T) {
	for _, tc := range []struct{ runtime, want string }{
		{"", "set Runtime=runc"},
		{"Runtime=cloud-hypervisor\n", "runsc or runc"},
	} {
		r, _ := runner(t, map[string]string{
			"out.cygateway":   "[Gateway]\nKind=direct\n",
			"dev.cynetwork":   "[Network]\nAttach=service:proxy\n",
			"proxy.cyservice": "[Service]\nSeedImage=img\n" + tc.runtime + "Gateway=out\nPort=8080\n",
		})
		r.State, r.Dataset = t.TempDir(), "tank/cyac"
		r.Runtimes = []driver.Runtime{&fakeRuntime{name: "cloud-hypervisor"}, &fakeRuntime{name: "runc", kind: driver.Netns}}
		if err := r.Start(context.Background(), "cyservice", "proxy"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.runtime, err, tc.want)
		}
	}
}
