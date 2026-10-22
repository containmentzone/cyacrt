// Package firecracker is the VM runtime: it boots a box's disk as
// /dev/vda under Firecracker, with the box's init (usually systemd) as
// PID 1. The guest kernel brings eth0 up from the ip= boot argument; the
// disk's networkd config carries the same address.
//
// With PVM set it is the "firecracker-pvm" runtime: the same VMM on a host
// without hardware virtualization (a cloud VM without nested virt) whose
// /dev/kvm comes from the out-of-tree kvm-pvm module. PVM needs a PVM guest
// kernel, and a normal guest kernel won't boot on it, so the two runtimes
// are mutually exclusive and each has its own kernel.
//
// TODO: run Firecracker under its jailer.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containmentzone/cyacrt/internal/driver"
)

// Runtime implements driver.Runtime.
type Runtime struct {
	Kernel string // uncompressed guest vmlinux
	PVM    bool
}

var _ driver.Runtime = Runtime{}

func (Runtime) LinkKind() driver.LinkKind        { return driver.Tap }
func socket(m *driver.Machine) string            { return filepath.Join(m.RunDir, "fc.sock") }
func pidFile(m *driver.Machine) string           { return filepath.Join(m.RunDir, "fc.pid") }
func (Runtime) Cleanup(m *driver.Machine)        {}
func (r Runtime) Running(m *driver.Machine) bool { return pid(m) > 0 }

func (r Runtime) Name() string {
	if r.PVM {
		return "firecracker-pvm"
	}
	return "firecracker"
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
	if _, err := exec.LookPath("firecracker"); err != nil {
		return err
	}
	_, err = os.Stat(r.Kernel)
	return err
}

func (r Runtime) Start(m *driver.Machine) error {
	mask := net.IP(net.CIDRMask(m.Addr.Prefix, 32))
	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw ip=%s::%s:%s:%s:eth0:off init=%s",
		m.Addr.IP, m.Addr.Gateway, mask, m.Name, m.Init[0])
	if len(m.Init) > 1 {
		bootArgs += " -- " + strings.Join(m.Init[1:], " ") // the kernel hands these to init as argv
	}
	ip := m.Addr.IP.As4()
	cfg := map[string]any{
		"boot-source": map[string]any{"kernel_image_path": r.Kernel, "boot_args": bootArgs},
		"drives": []map[string]any{{
			"drive_id": "rootfs", "path_on_host": m.Disk, "is_root_device": true, "is_read_only": false,
		}},
		"machine-config": map[string]any{"vcpu_count": m.CPUs, "mem_size_mib": m.MemMiB},
		"network-interfaces": []map[string]any{{
			"iface_id": "eth0", "host_dev_name": m.Link.HostIface,
			"guest_mac": fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3]),
		}},
	}
	cfgPath := filepath.Join(m.RunDir, "vm.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		return err
	}

	log, in, err := driver.OpenConsole(m)
	if err != nil {
		return err
	}
	defer log.Close()
	defer in.Close()
	// Firecracker's stdin and stdout are the guest serial console.
	cmd := exec.Command("firecracker", "--api-sock", socket(m), "--config-file", cfgPath)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive the CLI
	if err := cmd.Start(); err != nil {
		return err
	}
	p := cmd.Process.Pid
	cmd.Process.Release()
	if err := os.WriteFile(pidFile(m), []byte(strconv.Itoa(p)), 0o600); err != nil {
		return err
	}
	// Firecracker opens its API socket once the VM is configured.
	for range 50 {
		if _, err := os.Stat(socket(m)); err == nil {
			return nil
		}
		if pid(m) == 0 {
			return errors.New("firecracker exited")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("firecracker didn't come up")
}

func (Runtime) Stop(m *driver.Machine) error {
	// Ctrl-Alt-Del makes systemd reboot, and Firecracker exits on guest reboot.
	if err := api(m, http.MethodPut, "/actions", map[string]any{"action_type": "SendCtrlAltDel"}); err == nil {
		for range 60 {
			if pid(m) == 0 {
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if p := pid(m); p > 0 {
		return syscall.Kill(p, syscall.SIGKILL)
	}
	return nil
}

// Grow tells Firecracker the backing file changed size; the guest sees a
// resized virtio-blk device and grows its own filesystem, like a cloud VM.
func (Runtime) Grow(m *driver.Machine) error {
	return api(m, http.MethodPatch, "/drives/rootfs", map[string]any{
		"drive_id": "rootfs", "path_on_host": m.Disk,
	})
}

// Command reaches the guest over SSH with the key and pinned host key from
// seed time, so it needs sshd in the image. ssh joins the remote command
// into one string for the guest's shell, so each argument is quoted.
func (Runtime) Command(m *driver.Machine, args []string) (*exec.Cmd, error) {
	if _, err := os.Stat(m.SSHKey); err != nil {
		return nil, fmt.Errorf("%s has no SSH key (seeded before cyac supported exec in VMs): %w", m.Name, err)
	}
	sshArgs := []string{
		"-tt", "-i", m.SSHKey,
		"-o", "UserKnownHostsFile=" + m.KnownHosts, "-o", "StrictHostKeyChecking=yes",
		"-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5", "-o", "LogLevel=ERROR",
		"root@" + m.Addr.IP.String(), "--",
	}
	for _, a := range args {
		sshArgs = append(sshArgs, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return exec.Command("ssh", sshArgs...), nil
}

// pvmHost reports whether /dev/kvm would be served by kvm-pvm. It can't be
// loaded alongside kvm-intel or kvm-amd.
func pvmHost() bool {
	exists := func(mod string) bool { _, err := os.Stat("/sys/module/" + mod); return err == nil }
	return exists("kvm_pvm") && !exists("kvm_intel") && !exists("kvm_amd")
}

// pid returns the Firecracker pid if that process is still Firecracker.
func pid(m *driver.Machine) int {
	data, err := os.ReadFile(pidFile(m))
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(string(data))
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", p))
	if err != nil || strings.TrimSpace(string(comm)) != "firecracker" {
		return 0
	}
	return p
}

// api makes one request to Firecracker's API over its unix socket.
func api(m *driver.Machine, method, path string, body any) error {
	sock := socket(m)
	client := http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, "http://firecracker"+path, bytes.NewReader(data))
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
		var msg bytes.Buffer
		msg.ReadFrom(resp.Body)
		return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, msg.String())
	}
	return nil
}
