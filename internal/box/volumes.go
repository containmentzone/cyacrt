package box

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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

// Mountable is kept without the paths agent.VolumeOK refuses.
func Mountable(kept []string) []string {
	return slices.DeleteFunc(slices.Clone(kept), func(v string) bool { return !agent.VolumeOK(v) })
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
// changed. After a reseed, a new volume is filled from the old root
// (b.FillFrom) where that has the path, and from the new one where it
// doesn't. A box with no volumes gets no data disk. name must not be
// running.
func (s Store) FillVolumes(b *Box, want []string, sizeBytes int64, progress func(string)) (bool, error) {
	changed, err := s.fillVolumes(b, want, sizeBytes, progress)
	if err == nil && b.FillFrom != "" {
		b.FillFrom, changed = "", true
	}
	return changed, err
}

func (s Store) fillVolumes(b *Box, want []string, sizeBytes int64, progress func(string)) (bool, error) {
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
	// The data disk first, then the roots to fill from, in order.
	ms := []mount{{device(data), "rw"}}
	if b.FillFrom != "" && s.Exists(b.FillFrom) {
		ms = append(ms, mount{s.DiskPath(b.FillFrom), "ro"})
	}
	ms = append(ms, mount{s.DiskPath(b.Name), "ro"})
	err := s.mounted(b.Name, func(dirs []string) error {
		for _, v := range fresh {
			if err := fillVolume(sources(b.Kept, dirs[0], dirs[1:], v), dirs[0], v, progress); err != nil {
				return err
			}
			b.Kept = append(b.Kept, v)
		}
		return nil
	}, ms...)
	slices.SortFunc(b.Kept, cmpLen)
	return true, err
}

// growData grows the data disk data to sizeBytes, zvol and ext4, if it
// is smaller. The box isn't running, so the host resizes it offline,
// which wants a fresh fsck first. The filesystem is grown whenever it is
// smaller than the zvol, so a resize that failed is retried.
func growData(data string, sizeBytes int64) error {
	if err := grow(data, sizeBytes); err != nil {
		return err
	}
	vol, err := volsize(data)
	if err != nil {
		return err
	}
	fs, err := ext4Size(device(data))
	if err != nil || fs >= vol {
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

// ext4Size is the size in bytes of the ext4 on dev, from its superblock.
func ext4Size(dev string) (int64, error) {
	out, err := exec.Command("dumpe2fs", "-h", dev).Output()
	if err != nil {
		return 0, fmt.Errorf("dumpe2fs %s: %v", dev, err)
	}
	return parseExt4Size(string(out))
}

func parseExt4Size(s string) (int64, error) {
	var count, size int64
	for line := range strings.Lines(s) {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		switch k {
		case "Block count":
			count = n
		case "Block size":
			size = n
		}
	}
	if count == 0 || size == 0 {
		return 0, fmt.Errorf("no block count and size in dumpe2fs output")
	}
	return count * size, nil
}

// source is where a new volume's files may come from: path in the tree
// at dir.
type source struct{ dir, path string }

// sources lists where the new volume at path is filled from: the kept
// volume it is inside, if any, since that one's files are what the box
// sees there now; otherwise each root disk mounted at roots, in order.
func sources(kept []string, data string, roots []string, path string) []source {
	var parent string
	for _, k := range kept {
		if strings.HasPrefix(path, k+"/") && len(k) > len(parent) {
			parent = k
		}
	}
	if parent != "" {
		return []source{{filepath.Join(data, agent.VolumeDir(parent)), strings.TrimPrefix(path, parent)}}
	}
	var srcs []source
	for _, r := range roots {
		srcs = append(srcs, source{r, path})
	}
	return srcs
}

// fillVolume makes the directory on the data disk mounted at data for
// the volume at path, a copy of the first of srcs that has the files, or
// empty if none has. One the data disk already has
// is left as it is, if it is a directory: the box writes the data disk,
// and runc would follow a symlink there on the host. A copy goes to
// .fill-<dir> first, so one that died halfway is redone.
func fillVolume(srcs []source, data, path string, progress func(string)) error {
	dst := filepath.Join(data, agent.VolumeDir(path))
	if fi, err := os.Lstat(dst); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("volume %s: %s on the data disk is not a directory", path, agent.VolumeDir(path))
		}
		return nil
	}
	var src string
	for _, s := range srcs {
		p, err := realDir(s.dir, s.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		src = p
		break
	}
	if src == "" {
		progress("volume " + path + ": new and empty")
		return os.Mkdir(dst, 0o755)
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
