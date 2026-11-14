package runc

import (
	"slices"
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
