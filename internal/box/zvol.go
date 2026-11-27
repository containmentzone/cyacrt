package box

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/containmentzone/cyacrt/internal/sh"
)

// A workload's disk is a sparse zvol, <Dataset>/<name>, with volmode=dev:
// the host never scans what the workload writes to it for partitions.
// It is either a clone of its image's snapshot (see Images) or, if the
// workload asked for a smaller disk than the image's, a zvol of its own.

func (s Store) volume(name string) string { return s.Dataset + "/" + name }

// createVolume makes the sparse zvol vol, and its parent datasets if need
// be, and waits for udev to link its device. It fails if vol already
// exists: only -p on the parents, since `zfs create -p` succeeds on an
// existing target, and a seed would then mkfs over a disk it didn't make.
func createVolume(vol string, size int64) error {
	if err := sh.Run("zfs", "create", "-p", path.Dir(vol)); err != nil {
		return err
	}
	if err := sh.Run("zfs", "create", "-s", "-V", strconv.FormatInt(size, 10),
		"-o", "volmode=dev", vol); err != nil {
		return err
	}
	return waitDevice(vol)
}

// cloneVolume makes vol a clone of the snapshot snap, as createVolume
// makes a new one. vol starts with snap's size and contents.
func cloneVolume(snap, vol string) error {
	if err := sh.Run("zfs", "create", "-p", path.Dir(vol)); err != nil {
		return err
	}
	if err := sh.Run("zfs", "clone", "-o", "volmode=dev", snap, vol); err != nil {
		return err
	}
	return waitDevice(vol)
}

func device(vol string) string { return "/dev/zvol/" + vol }

func waitDevice(vol string) error {
	for range 100 {
		if _, err := os.Stat(device(vol)); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s did not appear", device(vol))
}

// Exists reports whether name has a zvol.
func (s Store) Exists(name string) bool {
	_, err := volsize(s.volume(name))
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
	cur, err := volsize(s.volume(name))
	if err != nil || size <= cur {
		return err
	}
	return sh.Run("zfs", "set", "volsize="+strconv.FormatInt(size, 10), s.volume(name))
}

func volsize(vol string) (int64, error) {
	out, err := exec.Command("zfs", "get", "-Hp", "-o", "value", "volsize", vol).Output()
	if err != nil {
		return 0, fmt.Errorf("zfs get volsize %s: %w", vol, err)
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}
