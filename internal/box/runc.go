package box

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// The container tier runs the box's disk with its own init (usually
// systemd) as PID 1.
// This is NOT a security boundary: systemd needs CAP_SYS_ADMIN and a
// writable cgroup subtree, so treat these boxes as trusted.

func containerID(b *Box) string { return "cyac-" + b.Name }

func startContainer(s Store, b *Box) error {
	dir := s.RunDir(b.Name)
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.Mkdir(rootfs, 0o755); err != nil {
		return err
	}
	if err := run("mount", "-o", "loop", s.DiskPath(b.Name), rootfs); err != nil {
		return err
	}
	netns, err := addNetns(b)
	if err != nil {
		return err
	}
	spec, err := json.MarshalIndent(containerSpec(b, netns), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), spec, 0o600); err != nil {
		return err
	}
	log, err := openLog(s, b)
	if err != nil {
		return err
	}
	defer log.Close()
	// Detached: the container's stdio is the log file, and runc returns once
	// /sbin/init is running.
	cmd := exec.Command("runc", "run", "--detach", "--bundle", dir, containerID(b))
	cmd.Stdout, cmd.Stderr = log, log
	return cmd.Run()
}

func stopContainer(s Store, b *Box) error {
	// SIGRTMIN+3 (37 on Linux) asks systemd for a clean halt; runc only
	// takes it by number.
	if err := run("runc", "kill", containerID(b), "37"); err != nil {
		return err
	}
	for range 60 {
		if !containerRunning(b) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return run("runc", "kill", containerID(b), "KILL")
}

func containerRunning(b *Box) bool {
	out, err := exec.Command("runc", "state", containerID(b)).Output()
	if err != nil {
		return false
	}
	var st specs.State
	return json.Unmarshal(out, &st) == nil && st.Status == specs.StateRunning
}

func containerSpec(b *Box, netnsPath string) *specs.Spec {
	caps := []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER", "CAP_FSETID",
		"CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP", "CAP_SETFCAP",
		"CAP_NET_BIND_SERVICE", "CAP_NET_RAW", "CAP_NET_ADMIN", "CAP_IPC_LOCK",
		"CAP_SYS_CHROOT", "CAP_SYS_ADMIN", "CAP_SYS_NICE", "CAP_SYS_RESOURCE",
		"CAP_MKNOD", "CAP_AUDIT_WRITE", "CAP_LEASE",
	}
	period := uint64(100_000)
	quota := int64(b.CPUs) * int64(period)
	mem := int64(b.MemMiB) << 20
	return &specs.Spec{
		Version:  specs.Version,
		Root:     &specs.Root{Path: "rootfs"},
		Hostname: b.Name,
		Process: &specs.Process{
			Args: b.Init,
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
				{Type: specs.NetworkNamespace, Path: netnsPath},
			},
			CgroupsPath: "/cyac/" + b.Name,
			Resources: &specs.LinuxResources{
				Memory: &specs.LinuxMemory{Limit: &mem},
				CPU:    &specs.LinuxCPU{Quota: &quota, Period: &period},
			},
			MaskedPaths:   []string{"/proc/kcore", "/proc/keys", "/proc/timer_list", "/sys/firmware"},
			ReadonlyPaths: []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"},
		},
	}
}
