package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containmentzone/cyacrt/internal/agent"
)

// mountVolumes bind-mounts each of paths into the root at /newroot from
// its directory on the data disk, /dev/vdb, an ext4 the host made and
// grows. discard hands what the box frees back to the host's zvol.
func mountVolumes(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	const data = "/data"
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("/dev/vdb", data, "ext4", 0, "discard"); err != nil {
		return fmt.Errorf("mount /dev/vdb: %w", err)
	}
	for _, p := range paths {
		dst := filepath.Join("/newroot", p)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("volume %s: %w", p, err)
		}
		if err := syscall.Mount(filepath.Join(data, agent.VolumeDir(p)), dst, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("volume %s: %w", p, err)
		}
	}
	// The bind mounts keep the data disk mounted.
	return syscall.Unmount(data, 0)
}
