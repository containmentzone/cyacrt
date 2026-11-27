package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/containmentzone/cyacrt/internal/agent"
)

// ext4ResizeFS is EXT4_IOC_RESIZE_FS, _IOW('f', 16, __u64): the ioctl
// resize2fs and systemd-growfs use to grow a mounted ext4.
const ext4ResizeFS = 0x40086610

// mountAny mounts dev at dir as the first filesystem type the kernel
// accepts, trying each block filesystem it knows, as busybox mount does
// without -t. A box may reformat its disk; this finds what it chose.
//
// opts are tried first, then no options, for a filesystem that knows
// none of them.
func mountAny(dev, dir, opts string) error {
	types, err := blockFilesystems()
	if err != nil {
		return err
	}
	for _, o := range slices.Compact([]string{opts, ""}) {
		for _, t := range types {
			if err = syscall.Mount(dev, dir, t, 0, o); err == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("mount %s: none of %v took it (last: %v)", dev, types, err)
}

// mountVolumes bind-mounts each of paths into the root at /newroot from
// its directory on the data disk, /dev/vdb, which is grown first if it
// is ext4. discard hands what the box frees back to the host's zvol.
func mountVolumes(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	const data = "/data"
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	if err := mountAny("/dev/vdb", data, "discard"); err != nil {
		return err
	}
	grow("/dev/vdb", data)
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

// blockFilesystems lists the filesystems the kernel can mount from a
// device, ext4 first, read from /proc/filesystems with proc mounted on
// the initramfs's /proc for the purpose.
func blockFilesystems() ([]string, error) {
	if err := os.MkdirAll("/proc", 0o755); err != nil {
		return nil, err
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return nil, fmt.Errorf("mount /proc: %w", err)
	}
	defer syscall.Unmount("/proc", 0)
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return nil, err
	}
	return parseFilesystems(string(data)), nil
}

// parseFilesystems reads /proc/filesystems: one type per line, after
// "nodev" if it needs no device. ext4 goes first, so an ext4 disk never
// mounts as ext2 or ext3.
func parseFilesystems(s string) []string {
	types := []string{}
	for line := range strings.Lines(s) {
		f := strings.Fields(line)
		if len(f) == 1 {
			types = append(types, f[0])
		}
	}
	if i := slices.Index(types, "ext4"); i > 0 {
		types = slices.Insert(slices.Delete(types, i, i+1), 0, "ext4")
	}
	return types
}

// grow grows the filesystem mounted at dir to fill dev, online, if it is
// ext4. Other filesystems are the box's to grow. A failure only costs the
// box its new space, so it is logged, not fatal.
func grow(dev, dir string) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		logf("grow %s: %v", dev, err)
		return
	}
	if st.Type != unix.EXT4_SUPER_MAGIC {
		logf("%s is not ext4; not growing it", dev)
		return
	}
	f, err := os.Open(dev)
	if err != nil {
		logf("grow %s: %v", dev, err)
		return
	}
	defer f.Close()
	var size uint64
	if err := ioctl(f.Fd(), unix.BLKGETSIZE64, unsafe.Pointer(&size)); err != nil {
		logf("grow %s: size: %v", dev, err)
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		logf("grow %s: %v", dev, err)
		return
	}
	defer d.Close()
	blocks := size / uint64(st.Bsize)
	if err := ioctl(d.Fd(), ext4ResizeFS, unsafe.Pointer(&blocks)); err != nil {
		logf("grow %s to %d blocks: %v", dev, blocks, err)
	}
}

func ioctl(fd uintptr, req uint, arg unsafe.Pointer) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), uintptr(arg)); e != 0 {
		return e
	}
	return nil
}
