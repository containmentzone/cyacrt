package runc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containmentzone/cyacrt/internal/driver"
)

func TestConfinedSpec(t *testing.T) {
	for _, confined := range []bool{false, true} {
		spec := Runtime{}.containerSpec(&driver.Machine{Name: "x", RunDir: t.TempDir(), CPUs: 1, MemMiB: 64, Confined: confined})
		caps := spec.Process.Capabilities
		for _, set := range [][]string{caps.Bounding, caps.Effective, caps.Permitted} {
			for _, c := range []string{"CAP_SYS_ADMIN", "CAP_NET_ADMIN", "CAP_NET_RAW", "CAP_MKNOD"} {
				if slices.Contains(set, c) == confined {
					t.Errorf("confined=%v: %s present=%v", confined, c, !confined)
				}
			}
		}
		if spec.Process.NoNewPrivileges != confined {
			t.Errorf("confined=%v: NoNewPrivileges=%v", confined, spec.Process.NoNewPrivileges)
		}
		cgroup := slices.ContainsFunc(spec.Mounts, func(m specs.Mount) bool { return m.Destination == "/sys/fs/cgroup" })
		if cgroup == confined {
			t.Errorf("confined=%v: cgroup mount=%v", confined, cgroup)
		}
		tun := slices.ContainsFunc(spec.Linux.Devices, func(d specs.LinuxDevice) bool { return d.Path == "/dev/net/tun" })
		if tun == confined {
			t.Errorf("confined=%v: /dev/net/tun=%v", confined, tun)
		}
	}
}

func TestGVisorCommands(t *testing.T) {
	m := &driver.Machine{Name: "svc"}
	for _, tc := range []struct {
		rt   Runtime
		want string
	}{
		{Runtime{}, "runc exec -t cyac-svc sh"},
		{Runtime{GVisor: true}, "runsc --overlay2=none exec cyac-svc sh"},
	} {
		cmd, err := tc.rt.Command(m, []string{"sh"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cmd.Args, " "); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.rt.Name(), got, tc.want)
		}
	}
}

func TestVolumeMounts(t *testing.T) {
	m := &driver.Machine{Name: "x", RunDir: "/run/x", CPUs: 1, MemMiB: 64,
		Data: "/dev/zvol/tank/boxes/x.data", Volumes: []string{"/home", "/var/lib/my-app"}}
	var got []string
	for _, mnt := range (Runtime{}).containerSpec(m).Mounts {
		if mnt.Type == "bind" {
			got = append(got, mnt.Source+" "+mnt.Destination)
		}
	}
	want := []string{"/run/x/data/home /home", `/run/x/data/var-lib-my\x2dapp /var/lib/my-app`}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCheckVolumes(t *testing.T) {
	data := t.TempDir()
	os.Mkdir(filepath.Join(data, "home"), 0o755)
	os.Symlink("/etc", filepath.Join(data, "srv"))
	if err := checkVolumes(data, []string{"/home"}); err != nil {
		t.Errorf("a directory: %v", err)
	}
	if err := checkVolumes(data, []string{"/home", "/srv"}); err == nil {
		t.Error("took a symlink on the data disk")
	}
}
