package cloudhypervisor

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/containmentzone/cyacrt/internal/driver"
)

// pid knows the VMM by its API socket, whatever argv[0] is (a wrapper
// script's shell, say), and not by name.
func TestPid(t *testing.T) {
	m := &driver.Machine{RunDir: t.TempDir()}
	other := &driver.Machine{RunDir: t.TempDir()}
	// A plain sleep with the socket as its argv[0]: no shell, which may
	// exec its last command and drop the args.
	cmd := exec.Command("sleep", "30")
	cmd.Args[0] = "path=" + socket(m)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	for _, mm := range []*driver.Machine{m, other} {
		if err := os.WriteFile(pidFile(mm), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Start returns once exec has begun, which can be before the kernel
	// has the new argv in /proc/<pid>/cmdline.
	for range 100 {
		if pid(m) != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid(m) != cmd.Process.Pid {
		t.Errorf("pid(m) = %d, want %d", pid(m), cmd.Process.Pid)
	}
	if pid(other) != 0 {
		t.Errorf("another machine's pid file matched a process that isn't its VMM")
	}
}
