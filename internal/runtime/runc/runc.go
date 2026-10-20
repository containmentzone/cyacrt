// Package runc is the container runtime: it runs a box's loop-mounted disk
// with its own init (usually systemd) as PID 1.
//
// This is NOT a security boundary: systemd needs CAP_SYS_ADMIN and a
// writable cgroup subtree, so treat these boxes as trusted.
package runc

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/sh"
)

// Runtime implements driver.Runtime.
type Runtime struct{}

var _ driver.Runtime = Runtime{}

func (Runtime) Name() string               { return "runc" }
func (Runtime) LinkKind() driver.LinkKind  { return driver.Netns }
func containerID(m *driver.Machine) string { return "cyac-" + m.Name }
func rootfs(m *driver.Machine) string      { return filepath.Join(m.RunDir, "rootfs") }

func (Runtime) Available() error {
	_, err := exec.LookPath("runc")
	return err
}

func (Runtime) Start(m *driver.Machine) error {
	if err := os.Mkdir(rootfs(m), 0o755); err != nil {
		return err
	}
	if err := sh.Run("mount", "-o", "loop", m.Disk, rootfs(m)); err != nil {
		return err
	}
	spec, err := json.MarshalIndent(containerSpec(m), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.RunDir, "config.json"), spec, 0o600); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(m.RunDir, "console.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	// Detached: the container's stdio is the log file, and runc returns once
	// init is running.
	cmd := exec.Command("runc", "run", "--detach", "--bundle", m.RunDir, containerID(m))
	cmd.Stdout, cmd.Stderr = log, log
	return cmd.Run()
}

func (r Runtime) Stop(m *driver.Machine) error {
	// SIGRTMIN+3 (37 on Linux) asks systemd for a clean halt; runc only
	// takes it by number.
	if err := sh.Run("runc", "kill", containerID(m), "37"); err != nil {
		return err
	}
	for range 60 {
		if !r.Running(m) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return sh.Run("runc", "kill", containerID(m), "KILL")
}

func (Runtime) Running(m *driver.Machine) bool {
	out, err := exec.Command("runc", "state", containerID(m)).Output()
	if err != nil {
		return false
	}
	var st specs.State
	return json.Unmarshal(out, &st) == nil && st.Status == specs.StateRunning
}

func (Runtime) Cleanup(m *driver.Machine) {
	sh.Run("runc", "delete", "--force", containerID(m))
	sh.Run("umount", rootfs(m))
}

// Grow resizes the mounted filesystem from the host. A container can't be
// trusted with its block device, so the box can't do this itself.
func (Runtime) Grow(m *driver.Machine) error {
	out, err := exec.Command("losetup", "-j", m.Disk, "-O", "NAME", "-n").Output()
	if err != nil {
		return err
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return fmt.Errorf("no loop device for %s", m.Disk)
	}
	if err := sh.Run("losetup", "-c", dev); err != nil {
		return err
	}
	return sh.Run("resize2fs", dev)
}

func (Runtime) Command(m *driver.Machine, args []string) (*exec.Cmd, error) {
	return exec.Command("runc", append([]string{"exec", "-t", containerID(m)}, args...)...), nil
}

func containerSpec(m *driver.Machine) *specs.Spec {
	caps := []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER", "CAP_FSETID",
		"CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP", "CAP_SETFCAP",
		"CAP_NET_BIND_SERVICE", "CAP_NET_RAW", "CAP_NET_ADMIN", "CAP_IPC_LOCK",
		"CAP_SYS_CHROOT", "CAP_SYS_ADMIN", "CAP_SYS_NICE", "CAP_SYS_RESOURCE",
		"CAP_MKNOD", "CAP_AUDIT_WRITE", "CAP_LEASE",
	}
	period := uint64(100_000)
	quota := int64(m.CPUs) * int64(period)
	mem := int64(m.MemMiB) << 20
	return &specs.Spec{
		Version:  specs.Version,
		Root:     &specs.Root{Path: "rootfs"},
		Hostname: m.Name,
		Process: &specs.Process{
			Args: m.Init,
			// systemd reads $container to skip units that make no sense here.
			Env:          []string{"container=cyac", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			Cwd:          "/",
			Capabilities: &specs.LinuxCapabilities{Bounding: caps, Effective: caps, Permitted: caps},
		},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			// With a cgroup namespace, systemd gets a writable view of its own subtree only.
			{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "rw"}},
			{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=755"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777"}},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace}, {Type: specs.IPCNamespace}, {Type: specs.UTSNamespace},
				{Type: specs.MountNamespace}, {Type: specs.CgroupNamespace},
				{Type: specs.NetworkNamespace, Path: m.Link.Netns},
			},
			CgroupsPath: "/cyac/" + m.Name,
			Resources: &specs.LinuxResources{
				Memory: &specs.LinuxMemory{Limit: &mem},
				CPU:    &specs.LinuxCPU{Quota: &quota, Period: &period},
			},
			MaskedPaths:   []string{"/proc/kcore", "/proc/keys", "/proc/timer_list", "/sys/firmware"},
			ReadonlyPaths: []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"},
		},
	}
}
