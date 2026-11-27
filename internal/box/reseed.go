package box

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/containmentzone/cyacrt/internal/sh"
)

// Reseed replaces b's disk with a new one seeded from b.Image, as Seed
// would make it, carrying over what identifies the box: its machine ID
// and SSH host keys (see carry). Everything else the old disk held is
// gone from the box. The new disk is made as <name>.new and swapped in
// by renaming, so the old one is untouched until then, and it stays as
// <name>.prev until the next reseed. The box must not be running.
func (s Store) Reseed(b *Box, sizeBytes int64, progress func(string)) error {
	if err := s.Recover(b.Name); err != nil {
		return err
	}
	next, prev := b.Name+".new", b.Name+".prev"
	if err := s.seed(b, s.volume(next), sizeBytes, progress); err != nil {
		s.Destroy(next)
		return err
	}
	progress("carrying the machine ID and SSH host keys over to " + s.volume(next))
	if err := s.carry(b.Name, next); err != nil {
		s.Destroy(next)
		return err
	}
	if err := s.Destroy(prev); err != nil {
		return err
	}
	if err := rename(s.volume(b.Name), s.volume(prev)); err != nil {
		return err
	}
	progress("replaced " + s.volume(b.Name) + "; the old disk is " + s.volume(prev) + " until the next reseed")
	return rename(s.volume(next), s.volume(b.Name))
}

// Recover cleans up after a reseed of name that died: it destroys a
// <name>.new, and if the disk was already renamed to <name>.prev but the
// new one never took its name, renames it back. The box is then as it
// was before the reseed.
func (s Store) Recover(name string) error {
	if err := s.Destroy(name + ".new"); err != nil {
		return err
	}
	if !s.Exists(name) && s.Exists(name+".prev") {
		return rename(s.volume(name+".prev"), s.volume(name))
	}
	return nil
}

// ResolveID returns the ID of the image ref names now, as Seed would
// record it in Box.ImageID, without unpacking it.
func ResolveID(ref string) (string, error) {
	img, err := pull(ref)
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
	}
	var id v1.Hash
	if id, err = img.ConfigName(); err != nil {
		return "", err
	}
	return id.String(), nil
}

// carry mounts disk from read-only and disk to, and copies what
// identifies the box from one to the other (see carryFiles).
func (s Store) carry(from, to string) error {
	dir, err := os.MkdirTemp(filepath.Join(s.Dir, "disks"), to+"-")
	if err != nil {
		return err
	}
	// os.Remove, never RemoveAll: if an unmount failed, the dirs still
	// hold the disks.
	defer os.Remove(dir)
	src, dst := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	for _, d := range []string{src, dst} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
		defer os.Remove(d)
	}
	if err := sh.Run("mount", "-o", "ro", device(s.volume(from)), src); err != nil {
		return err
	}
	defer sh.Run("umount", src)
	if err := sh.Run("mount", device(s.volume(to)), dst); err != nil {
		return err
	}
	defer sh.Run("umount", dst)
	return carryFiles(src, dst)
}

// carryFiles copies the machine ID and SSH host keys from the tree at
// src to the one at dst, where they replace the image's. Both trees are
// box content, so every path goes through os.Root, and only regular files
// are copied.
func carryFiles(src, dst string) error {
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer to.Close()
	names := []string{"etc/machine-id"}
	if keys, err := fs.ReadDir(from.FS(), "etc/ssh"); err == nil {
		for _, k := range keys {
			if strings.HasPrefix(k.Name(), "ssh_host_") {
				names = append(names, "etc/ssh/"+k.Name())
			}
		}
	}
	for _, name := range names {
		fi, err := from.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		data, err := from.ReadFile(name)
		if err != nil {
			return err
		}
		if err := to.MkdirAll(path.Dir(name), 0o755); err != nil {
			return err
		}
		if err := to.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		f, err := to.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
