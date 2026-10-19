package box

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tiers. The same disk boots under either.
const (
	TierVM        = "vm"        // Firecracker; needs /dev/kvm. Strong isolation.
	TierContainer = "container" // runc with systemd as PID 1. Trusted boxes only.
)

// KernelPath is the guest kernel for the VM tier.
func (s Store) KernelPath() string { return filepath.Join(s.Dir, "vmlinux") }

// DetectTier picks the VM tier when this host can run it, else the container
// tier. ALTEXE_TIER overrides the choice.
func (s Store) DetectTier() string {
	if t := os.Getenv("ALTEXE_TIER"); t != "" {
		return t
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return TierContainer
	}
	kvm.Close()
	if _, err := exec.LookPath("firecracker"); err != nil {
		return TierContainer
	}
	if _, err := os.Stat(s.KernelPath()); err != nil {
		return TierContainer
	}
	return TierVM
}

// runningTier reports which tier a box is running under, or "" if stopped.
func (s Store) runningTier(b *Box) string {
	t, err := os.ReadFile(filepath.Join(s.RunDir(b.Name), "tier"))
	if err != nil {
		return ""
	}
	switch tier := string(t); tier {
	case TierVM:
		if vmRunning(s, b) {
			return tier
		}
	case TierContainer:
		if containerRunning(b) {
			return tier
		}
	}
	return ""
}

// Status is "stopped" or "running (<tier>)".
func (s Store) Status(b *Box) string {
	if t := s.runningTier(b); t != "" {
		return "running (" + t + ")"
	}
	return "stopped"
}

func (s Store) Start(b *Box) error {
	if t := s.runningTier(b); t != "" {
		return fmt.Errorf("%s is already running (%s)", b.Name, t)
	}
	s.cleanup(b) // leftovers from a crash or host reboot
	if err := EnsureNetwork(); err != nil {
		return err
	}
	dir := s.RunDir(b.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tier := s.DetectTier()
	if err := os.WriteFile(filepath.Join(dir, "tier"), []byte(tier), 0o600); err != nil {
		return err
	}
	var err error
	switch tier {
	case TierVM:
		err = startVM(s, b)
	case TierContainer:
		err = startContainer(s, b)
	default:
		err = fmt.Errorf("unknown tier %q", tier)
	}
	if err != nil {
		console, _ := os.ReadFile(filepath.Join(dir, "console.log"))
		if len(console) > 2048 {
			console = console[len(console)-2048:]
		}
		s.cleanup(b)
		return fmt.Errorf("start %s (%s): %w\n%s", b.Name, tier, err, console)
	}
	return nil
}

func (s Store) Stop(b *Box) error {
	var err error
	switch s.runningTier(b) {
	case TierVM:
		err = stopVM(s, b)
	case TierContainer:
		err = stopContainer(s, b)
	default:
		return fmt.Errorf("%s is not running", b.Name)
	}
	s.cleanup(b)
	return err
}

// cleanup removes everything a run leaves behind. Safe to call when stopped.
func (s Store) cleanup(b *Box) {
	run("runc", "delete", "--force", containerID(b))
	run("umount", filepath.Join(s.RunDir(b.Name), "rootfs"))
	removeNet(b)
	os.RemoveAll(s.RunDir(b.Name))
}

// Resize grows a box's disk. Shrinking is not supported.
func (s Store) Resize(b *Box, sizeBytes int64) error {
	disk := s.DiskPath(b.Name)
	fi, err := os.Stat(disk)
	if err != nil {
		return err
	}
	if sizeBytes <= fi.Size() {
		return errors.New("new size must be larger than the current size")
	}
	if err := os.Truncate(disk, sizeBytes); err != nil {
		return err
	}
	switch s.runningTier(b) {
	case TierVM:
		// The guest sees the bigger block device and grows its own filesystem,
		// like a cloud VM would.
		return vmRescanDisk(s, b)
	case TierContainer:
		// A container can't be trusted with its block device, so the host
		// grows the mounted filesystem online.
		out, err := exec.Command("losetup", "-j", disk, "-O", "NAME", "-n").Output()
		if err != nil {
			return err
		}
		dev := strings.TrimSpace(string(out))
		if dev == "" {
			return fmt.Errorf("no loop device for %s", disk)
		}
		if err := run("losetup", "-c", dev); err != nil {
			return err
		}
		return run("resize2fs", dev)
	default:
		if err := run("e2fsck", "-f", "-p", disk); err != nil {
			return err
		}
		return run("resize2fs", disk)
	}
}

// Remove deletes a stopped box and its disk.
func (s Store) Remove(b *Box) error {
	if t := s.runningTier(b); t != "" {
		return fmt.Errorf("%s is running; stop it first", b.Name)
	}
	s.cleanup(b)
	if err := os.Remove(s.DiskPath(b.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return s.Delete(b.Name)
}

// Exec runs a command inside a running box with the caller's terminal.
func (s Store) Exec(b *Box, args []string) error {
	switch s.runningTier(b) {
	case TierContainer:
		cmd := exec.Command("runc", append([]string{"exec", "-t", containerID(b)}, args...)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	case TierVM:
		return fmt.Errorf("exec isn't supported for VMs yet; if the image runs sshd, use ssh root@%s", b.IP())
	default:
		return fmt.Errorf("%s is not running", b.Name)
	}
}

// openLog opens the box's console log, which collects the init's output.
func openLog(s Store, b *Box) (*os.File, error) {
	return os.OpenFile(filepath.Join(s.RunDir(b.Name), "console.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}
