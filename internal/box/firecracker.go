package box

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// The VM tier boots the box's disk as /dev/vda under Firecracker, with the
// box's init (usually systemd) as PID 1. The guest kernel brings eth0 up from the
// ip= boot argument; the disk's networkd config carries the same address.
//
// TODO: run Firecracker under its jailer.

func fcSocket(s Store, b *Box) string  { return filepath.Join(s.RunDir(b.Name), "fc.sock") }
func fcPidFile(s Store, b *Box) string { return filepath.Join(s.RunDir(b.Name), "fc.pid") }

func startVM(s Store, b *Box) error {
	if err := addTap(b); err != nil {
		return err
	}
	ip := b.IP()
	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw ip=%s::%s:255.255.0.0:%s:eth0:off init=%s",
		ip, HostIP, b.Name, b.Init[0])
	if len(b.Init) > 1 {
		bootArgs += " -- " + strings.Join(b.Init[1:], " ") // the kernel hands these to init as argv
	}
	cfg := map[string]any{
		"boot-source": map[string]any{"kernel_image_path": s.KernelPath(), "boot_args": bootArgs},
		"drives": []map[string]any{{
			"drive_id": "rootfs", "path_on_host": s.DiskPath(b.Name), "is_root_device": true, "is_read_only": false,
		}},
		"machine-config": map[string]any{"vcpu_count": b.CPUs, "mem_size_mib": b.MemMiB},
		"network-interfaces": []map[string]any{{
			"iface_id": "eth0", "host_dev_name": b.ifName(),
			"guest_mac": fmt.Sprintf("06:00:0a:58:%02x:%02x", byte(b.ID>>8), byte(b.ID)),
		}},
	}
	cfgPath := filepath.Join(s.RunDir(b.Name), "vm.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		return err
	}

	log, err := openLog(s, b)
	if err != nil {
		return err
	}
	defer log.Close()
	// Firecracker's stdout is the guest serial console.
	cmd := exec.Command("firecracker", "--api-sock", fcSocket(s, b), "--config-file", cfgPath)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive the CLI
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	if err := os.WriteFile(fcPidFile(s, b), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return err
	}
	// Firecracker opens its API socket once the VM is configured.
	for range 50 {
		if _, err := os.Stat(fcSocket(s, b)); err == nil {
			return nil
		}
		if !vmRunning(s, b) {
			return fmt.Errorf("firecracker exited; see %s", log.Name())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("firecracker didn't come up; see %s", log.Name())
}

func stopVM(s Store, b *Box) error {
	// Ctrl-Alt-Del makes systemd reboot, and Firecracker exits on guest reboot.
	if err := fcAPI(s, b, http.MethodPut, "/actions", map[string]any{"action_type": "SendCtrlAltDel"}); err == nil {
		for range 60 {
			if !vmRunning(s, b) {
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if pid := vmPid(s, b); pid > 0 {
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

// vmPid returns the Firecracker pid if that process is still Firecracker.
func vmPid(s Store, b *Box) int {
	data, err := os.ReadFile(fcPidFile(s, b))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(string(data))
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil || strings.TrimSpace(string(comm)) != "firecracker" {
		return 0
	}
	return pid
}

func vmRunning(s Store, b *Box) bool { return vmPid(s, b) > 0 }

// vmRescanDisk tells Firecracker the backing file changed size, which the
// guest sees as a resized virtio-blk device.
func vmRescanDisk(s Store, b *Box) error {
	return fcAPI(s, b, http.MethodPatch, "/drives/rootfs", map[string]any{
		"drive_id": "rootfs", "path_on_host": s.DiskPath(b.Name),
	})
}

// fcAPI makes one request to Firecracker's API over its unix socket.
func fcAPI(s Store, b *Box, method, path string, body any) error {
	sock := fcSocket(s, b)
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
