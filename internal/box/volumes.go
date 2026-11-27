package box

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containmentzone/cyacrt/internal/agent"
	"github.com/containmentzone/cyacrt/internal/sh"
)

// Volumes is every volume b's data disk should hold: the ones it holds
// already, its image's VOLUMEs and extra (the file's Volume=s), each
// once, shortest path first, so a volume inside another mounts after it.
// An image's VOLUME that can't be one (see agent.VolumeOK) is left out.
func Volumes(b *Box, extra []string) []string {
	var all []string
	for _, v := range slices.Concat(b.Kept, b.Volumes, extra) {
		if v = filepath.Clean(v); agent.VolumeOK(v) {
			all = append(all, v)
		}
	}
	slices.SortFunc(all, cmpLen)
	return slices.Compact(all)
}

func cmpLen(x, y string) int {
	if len(x) != len(y) {
		return len(x) - len(y)
	}
	return strings.Compare(x, y)
}

// FillVolumes readies name's data disk, <name>.data, for want (see
// Volumes): it makes it, an empty ext4 of sizeBytes, if there is none,
// and grows it to sizeBytes if there is. A volume b.Kept lacks gets its
// directory there filled with what the root disk has at its path, as
// Docker fills a new volume from the image: copied, not moved, so the
// root keeps its copy under the mount, until the next reseed. Then it
// joins b.Kept, and is never filled again. It reports whether b.Kept
// changed. A box with no volumes gets no data disk. name must not be
// running.
func (s Store) FillVolumes(b *Box, want []string, sizeBytes int64, progress func(string)) (bool, error) {
	if len(want) == 0 {
		return false, nil
	}
	data := s.volume(b.Name + ".data")
	if !s.Exists(b.Name + ".data") {
		// Made as <name>.data.new and renamed once it has a filesystem,
		// so a start that dies halfway leaves no unformatted data disk.
		progress("making the data disk " + data)
		if err := s.Destroy(b.Name + ".data.new"); err != nil {
			return false, err
		}
		next := data + ".new"
		if err := createVolume(next, sizeBytes); err != nil {
			return false, err
		}
		if out, err := exec.Command("mkfs.ext4", "-q", "-L", "cyac-data", device(next)).CombinedOutput(); err != nil {
			sh.Run("zfs", "destroy", next)
			return false, fmt.Errorf("mkfs.ext4: %v: %s", err, out)
		}
		if err := rename(next, data); err != nil {
			return false, err
		}
	} else if err := growData(data, sizeBytes); err != nil {
		return false, err
	}
	var fresh []string
	for _, v := range want {
		if !slices.Contains(b.Kept, v) {
			fresh = append(fresh, v)
		}
	}
	if len(fresh) == 0 {
		return false, nil
	}
	err := s.mounted(b.Name, func(dirs []string) error {
		for _, v := range fresh {
			if err := fillVolume(dirs[0], dirs[1], v, progress); err != nil {
				return err
			}
			b.Kept = append(b.Kept, v)
		}
		return nil
	}, mount{s.DiskPath(b.Name), "ro"}, mount{device(data), "rw"})
	slices.SortFunc(b.Kept, cmpLen)
	return true, err
}

// growData grows the data disk data to sizeBytes, zvol and ext4, if it
// is smaller. The box isn't running, so the host resizes it offline,
// which wants a fresh fsck first.
func growData(data string, sizeBytes int64) error {
	cur, err := volsize(data)
	if err != nil || sizeBytes <= cur {
		return err
	}
	if err := grow(data, sizeBytes); err != nil {
		return err
	}
	if out, err := exec.Command("e2fsck", "-fp", device(data)).CombinedOutput(); err != nil {
		return fmt.Errorf("e2fsck %s: %v: %s", data, err, out)
	}
	if out, err := exec.Command("resize2fs", device(data)).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs %s: %v: %s", data, err, out)
	}
	return nil
}

// fillVolume makes the directory on the data disk mounted at data for
// the volume at path, a copy of path in the root disk mounted at root,
// or empty if the root has nothing there. One the data disk already has
// is left as it is, if it is a directory: the box writes the data disk,
// and runc would follow a symlink there on the host. A copy goes to
// .fill-<dir> first, so one that died halfway is redone.
func fillVolume(root, data, path string, progress func(string)) error {
	dst := filepath.Join(data, agent.VolumeDir(path))
	if fi, err := os.Lstat(dst); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("volume %s: %s on the data disk is not a directory", path, agent.VolumeDir(path))
		}
		return nil
	}
	src, err := realDir(root, path)
	if errors.Is(err, fs.ErrNotExist) {
		progress("volume " + path + ": new and empty")
		return os.Mkdir(dst, 0o755)
	}
	if err != nil {
		return err
	}
	progress("volume " + path + ": filling it from the root disk")
	// VolumeDir never starts a name with ".", so this can't be a volume.
	tmp := filepath.Join(data, ".fill-"+agent.VolumeDir(path))
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if out, err := exec.Command("cp", "-a", "--", src, tmp).CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("fill volume %s: %v: %s", path, err, out)
	}
	return os.Rename(tmp, dst)
}

// realDir returns path inside the tree at root, if every part of it is a
// directory there and none a symlink, which would lead the copy out of
// the tree.
func realDir(root, path string) (string, error) {
	p := root
	for part := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("volume %s: %s in the root disk is not a directory", path, strings.TrimPrefix(p, root))
		}
	}
	return p, nil
}
