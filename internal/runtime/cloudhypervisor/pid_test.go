package cloudhypervisor

import (
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/containmentzone/cyacrt/internal/driver"
)

// pid knows the VMM by its API socket, whatever argv[0] is (a wrapper
// script's shell, say), and not by name.
func TestPid(t *testing.T) {
	m := &driver.Machine{RunDir: t.TempDir()}
	other := &driver.Machine{RunDir: t.TempDir()}
	cmd := exec.Command("sh", "-c", "sleep 30", "--api-socket", "path="+socket(m))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	for _, mm := range []*driver.Machine{m, other} {
		if err := os.WriteFile(pidFile(mm), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if pid(m) != cmd.Process.Pid {
		t.Errorf("pid(m) = %d, want %d", pid(m), cmd.Process.Pid)
	}
	if pid(other) != 0 {
		t.Errorf("another machine's pid file matched a process that isn't its VMM")
	}
}
