package runc

import (
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
