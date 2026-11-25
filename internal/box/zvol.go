package box

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/containmentzone/cyacrt/internal/sh"
)

// A workload's disk is a sparse zvol, <Dataset>/<name>, with volmode=dev:
// the host never scans what the workload writes to it for partitions.

func (s Store) volume(name string) string { return s.Dataset + "/" + name }

// createVolume makes name's zvol, and its parent datasets if need be, and
// waits for udev to link its device. It fails if the zvol already exists:
// only -p on the parents, since `zfs create -p` succeeds on an existing
// target, and a seed would then mkfs over a disk it didn't make.
func (s Store) createVolume(name string, size int64) error {
	if err := sh.Run("zfs", "create", "-p", s.Dataset); err != nil {
		return err
	}
	if err := sh.Run("zfs", "create", "-s", "-V", strconv.FormatInt(size, 10),
		"-o", "volmode=dev", s.volume(name)); err != nil {
		return err
	}
	for range 100 {
		if _, err := os.Stat(s.DiskPath(name)); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s did not appear", s.DiskPath(name))
}

// Exists reports whether name has a zvol.
func (s Store) Exists(name string) bool {
	_, err := s.volsize(name)
	return err == nil
}

// Destroy removes name's zvol, if it has one.
func (s Store) Destroy(name string) error {
	if !s.Exists(name) {
		return nil
	}
	return sh.Run("zfs", "destroy", s.volume(name))
}

// Grow raises name's zvol to size bytes. It never shrinks one, which would
// cut off the end of its filesystem. The filesystem grows separately: a VM
// grows its own, and runc grows a container's on start.
func (s Store) Grow(name string, size int64) error {
	cur, err := s.volsize(name)
	if err != nil || size <= cur {
		return err
	}
	return sh.Run("zfs", "set", "volsize="+strconv.FormatInt(size, 10), s.volume(name))
}

func (s Store) volsize(name string) (int64, error) {
	out, err := exec.Command("zfs", "get", "-Hp", "-o", "value", "volsize", s.volume(name)).Output()
	if err != nil {
		return 0, fmt.Errorf("zfs get volsize %s: %w", s.volume(name), err)
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}
