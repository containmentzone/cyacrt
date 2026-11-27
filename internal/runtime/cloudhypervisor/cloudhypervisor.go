// Package cloudhypervisor is the VM runtime: it boots a box's disk as
// /dev/vda under Cloud Hypervisor. The VM starts in an initramfs holding
// cyac-init, which mounts the disk, starts an exec agent on vsock, and runs
// the box's init (usually systemd) or command (see cmd/cyac-init). The
// guest kernel brings eth0 up from the ip= boot argument; the disk's
// networkd config carries the same address.
//
// A guest reboot resets the VM in place. A guest poweroff (ACPI) ends the
// cloud-hypervisor process, which is how its unit sees a box stop itself.
//
// TODO: confine Cloud Hypervisor with its --landlock rules.
package cloudhypervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containmentzone/cyacrt/internal/agent"
	"github.com/containmentzone/cyacrt/internal/driver"
)

const binary = "cloud-hypervisor"

// Runtime implements driver.Runtime.
type Runtime struct {
	Kernel string // guest kernel: a PVH vmlinux on x86_64
	Init   string // static cyac-init binary
}

var _ driver.Runtime = Runtime{}

func (Runtime) LinkKind() driver.LinkKind        { return driver.Tap }
func socket(m *driver.Machine) string            { return filepath.Join(m.RunDir, "ch.sock") }
func pidFile(m *driver.Machine) string           { return filepath.Join(m.RunDir, "ch.pid") }
func vsock(m *driver.Machine) string             { return filepath.Join(m.RunDir, "v.sock") }
func (Runtime) Cleanup(m *driver.Machine)        {}
func (r Runtime) Running(m *driver.Machine) bool { return pid(m) > 0 }
func (Runtime) Name() string                     { return binary }

func (r Runtime) Available() error {
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	kvm.Close()
	if _, err := exec.LookPath(binary); err != nil {
		return err
	}
	if _, err = os.Stat(r.Init); err != nil {
		return err
	}
	_, err = os.Stat(r.Kernel)
	return err
}

func (r Runtime) Start(m *driver.Machine) error {
	initrd := filepath.Join(m.RunDir, "initrd")
	f, err := os.OpenFile(initrd, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = agent.WriteInitrd(f, r.Init, m.Init)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	mask := net.IP(net.CIDRMask(m.Addr.Prefix, 32))
	// No panic=: a panicking guest would reboot in place, again and again.
	var gw string // none on a sealed network
	if m.Addr.Gateway.IsValid() {
		gw = m.Addr.Gateway.String()
	}
	cmdline := fmt.Sprintf("console=ttyS0 ip=%s::%s:%s:%s:eth0:off",
		m.Addr.IP, gw, mask, m.Name)

	log, in, err := driver.OpenConsole(m)
	if err != nil {
		return err
	}
	defer log.Close()
	defer in.Close()
	// With --serial tty, Cloud Hypervisor's stdin and stdout are the guest
	// serial console.
	cmd := exec.Command(binary,
		"--api-socket", "path="+socket(m),
		"--kernel", r.Kernel,
		"--initramfs", initrd,
		"--cmdline", cmdline,
		"--cpus", fmt.Sprintf("boot=%d", m.CPUs),
		"--memory", fmt.Sprintf("size=%dM", m.MemMiB),
		// image_type=raw: without it, Cloud Hypervisor guards sector 0 against
		// a guest rewriting the format probe, and ext4 rewrites block 0 (it holds
		// the superblock) on every mount.
		"--disk", "path="+m.Disk+",id=rootfs,image_type=raw",
		"--net", fmt.Sprintf("tap=%s,mac=%s,id=eth0", m.Link.HostIface, m.MAC),
		"--vsock", "cid=3,socket="+vsock(m),
		"--serial", "tty",
		"--console", "off",
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, log, log
	// Its own session, so a signal to the daemon's process group (Ctrl-C
	// on a foreground daemon) doesn't reach it; the daemon stops it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	p := cmd.Process.Pid
	// Reap it when it exits; a zombie still has /proc/<pid>/cmdline, so
	// pid() would report it running.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if err := os.WriteFile(pidFile(m), []byte(strconv.Itoa(p)), 0o600); err != nil {
		return err
	}
	for range 50 {
		if _, err := os.Stat(socket(m)); err == nil {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("cloud-hypervisor exited: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("cloud-hypervisor didn't come up")
}

func (Runtime) Stop(m *driver.Machine) error {
	// The agent asks PID 1 to power off (systemd or cyac-init alike). The
	// ACPI power button covers a guest whose agent is gone, if systemd-logind
	// runs there. Either way the VMM exits once the guest powers off.
	agent.PowerOff(vsock(m))
	api(m, "vm.power-button")
	for range 60 {
		if pid(m) == 0 {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if p := pid(m); p > 0 {
		return syscall.Kill(p, syscall.SIGKILL)
	}
	return nil
}

// Command runs args through the guest's agent, by running "cyac
// agent-exec" (agent.Run) on the daemon's pty.
func (Runtime) Command(m *driver.Machine, args []string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(self, append([]string{"agent-exec", vsock(m)}, args...)...), nil
}

// pid returns the Cloud Hypervisor pid if that process is still the one we
// started: its comm is cut to 15 bytes, so this checks argv[0].
func pid(m *driver.Machine) int {
	data, err := os.ReadFile(pidFile(m))
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(string(data))
	argv, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p))
	if err != nil {
		return 0
	}
	// Ours if it was started with m's API socket. That holds through a
	// wrapper script (argv[0] is the shell until it execs the VMM) and
	// rules out an unrelated process that reused the pid.
	if !bytes.Contains(argv, []byte("path="+socket(m)+"\x00")) {
		return 0
	}
	return p
}

// api makes one PUT request without a body to Cloud Hypervisor's API over its unix socket.
func api(m *driver.Machine, action string) error {
	sock := socket(m)
	client := http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}
	req, err := http.NewRequest(http.MethodPut, "http://localhost/api/v1/"+action, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var msg strings.Builder
		io.Copy(&msg, resp.Body)
		return fmt.Errorf("cloud-hypervisor %s: %s: %s", action, resp.Status, msg.String())
	}
	return nil
}
