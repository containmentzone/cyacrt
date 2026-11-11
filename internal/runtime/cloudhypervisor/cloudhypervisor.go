// Package cloudhypervisor is the VM runtime: it boots a box's disk as
// /dev/vda under Cloud Hypervisor. The VM starts in an initramfs holding
// cyac-init, which mounts the disk, starts an exec agent on vsock, and runs
// the box's init (usually systemd) or command (see cmd/cyac-init). The
// guest kernel brings eth0 up from the ip= boot argument; the disk's
// networkd config carries the same address.
//
// A guest reboot resets the VM in place. A guest poweroff (ACPI) ends the
// cloud-hypervisor process, which is how the daemon sees a box stop itself.
//
// With PVM set it is the "cloud-hypervisor-pvm" runtime: the same VMM on a
// host without hardware virtualization (a cloud VM without nested virt)
// whose /dev/kvm comes from the out-of-tree kvm-pvm module. PVM needs a PVM
// guest kernel, and a normal guest kernel won't boot on it, so the two
// runtimes are mutually exclusive and each has its own kernel.
//
// TODO: confine Cloud Hypervisor with its --landlock rules.
package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
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
	PVM    bool
}

var _ driver.Runtime = Runtime{}

func (Runtime) LinkKind() driver.LinkKind        { return driver.Tap }
func socket(m *driver.Machine) string            { return filepath.Join(m.RunDir, "ch.sock") }
func pidFile(m *driver.Machine) string           { return filepath.Join(m.RunDir, "ch.pid") }
func vsock(m *driver.Machine) string             { return filepath.Join(m.RunDir, "v.sock") }
func (Runtime) Cleanup(m *driver.Machine)        {}
func (r Runtime) Running(m *driver.Machine) bool { return pid(m) > 0 }

func (r Runtime) Name() string {
	if r.PVM {
		return binary + "-pvm"
	}
	return binary
}

func (r Runtime) Available() error {
	if pvm := pvmHost(); pvm && !r.PVM {
		return errors.New("/dev/kvm is kvm-pvm, which needs the PVM guest kernel")
	} else if !pvm && r.PVM {
		return errors.New("kvm-pvm is not loaded")
	}
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
	cmdline := fmt.Sprintf("console=ttyS0 ip=%s::%s:%s:%s:eth0:off",
		m.Addr.IP, m.Addr.Gateway, mask, m.Name)
	cpus := fmt.Sprintf("boot=%d", m.CPUs)
	if r.PVM {
		// virt-pvm's guide caps the guest's physical address width for
		// Cloud Hypervisor; PVM guests can't use the host's full width.
		cpus += ",max_phys_bits=43"
	}
	ip := m.Addr.IP.As4()
	mac := fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3])

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
		"--cpus", cpus,
		"--memory", fmt.Sprintf("size=%dM", m.MemMiB),
		"--disk", "path="+m.Disk+",id=rootfs",
		"--net", fmt.Sprintf("tap=%s,mac=%s,id=eth0", m.Link.HostIface, mac),
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
	go cmd.Wait()
	if err := os.WriteFile(pidFile(m), []byte(strconv.Itoa(p)), 0o600); err != nil {
		return err
	}
	for range 50 {
		if _, err := os.Stat(socket(m)); err == nil {
			return nil
		}
		if pid(m) == 0 {
			return errors.New("cloud-hypervisor exited")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("cloud-hypervisor didn't come up")
}

func (Runtime) Stop(m *driver.Machine) error {
	// The agent asks PID 1 to power off (systemd or cyac-init alike). The
	// ACPI power button covers a guest whose agent is gone, if systemd-logind
	// runs there. Either way the VMM exits once the guest powers off.
	agent.PowerOff(vsock(m))
	api(m, "vm.power-button", nil)
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

// Grow tells Cloud Hypervisor the backing file changed size; the guest sees
// a resized virtio-blk device and grows its own filesystem, like a cloud VM.
func (Runtime) Grow(m *driver.Machine) error {
	fi, err := os.Stat(m.Disk)
	if err != nil {
		return err
	}
	return api(m, "vm.resize-disk", map[string]any{"id": "rootfs", "desired_size": fi.Size()})
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

// pvmHost reports whether /dev/kvm would be served by kvm-pvm. It can't be
// loaded alongside kvm-intel or kvm-amd.
func pvmHost() bool {
	exists := func(mod string) bool { _, err := os.Stat("/sys/module/" + mod); return err == nil }
	return exists("kvm_pvm") && !exists("kvm_intel") && !exists("kvm_amd")
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
	if argv0, _, _ := bytes.Cut(argv, []byte{0}); filepath.Base(string(argv0)) != binary {
		return 0
	}
	return p
}

// api makes one PUT request to Cloud Hypervisor's API over its unix socket.
func api(m *driver.Machine, action string, body any) error {
	sock := socket(m)
	client := http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPut, "http://localhost/api/v1/"+action, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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
