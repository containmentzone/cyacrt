package box

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/containmentzone/cyacrt/internal/sh"
)

// Reseed replaces b's disk with a new one seeded from b.Image, as Seed
// would make it, carrying over the box's SSH host keys (see carry); its
// machine ID is the host's to write (Box.MachineID). Everything else the old disk held is
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
	progress("carrying the SSH host keys over to " + s.volume(next))
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
	return s.mounted(to, func(dirs []string) error { return carryFiles(dirs[0], dirs[1]) },
		mount{device(s.volume(from)), "ro"}, mount{device(s.volume(to)), "rw"})
}

type mount struct{ dev, opts string }

// mounted mounts each of ms on a dir of its own under <dir>/disks, calls
// fn with those dirs, and unmounts them again.
func (s Store) mounted(name string, fn func(dirs []string) error, ms ...mount) error {
	dir, err := os.MkdirTemp(filepath.Join(s.Dir, "disks"), name+"-")
	if err != nil {
		return err
	}
	// os.Remove, never RemoveAll: if an unmount failed, the dirs still
	// hold the disks.
	defer os.Remove(dir)
	var dirs []string
	for i, m := range ms {
		d := filepath.Join(dir, strconv.Itoa(i))
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
		defer os.Remove(d)
		if err := sh.Run("mount", "-o", m.opts, m.dev, d); err != nil {
			return err
		}
		defer sh.Run("umount", d)
		dirs = append(dirs, d)
	}
	return fn(dirs)
}

// carryFiles copies the SSH host keys from the tree at
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
	var names []string
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
		if err := to.MkdirAll(path.Dir(name), 0o755); err != nil {
			return err
		}
		if err := to.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := copyFile(from, to, name, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies name from one tree to the other, where it must not exist.
func copyFile(from, to *os.Root, name string, perm fs.FileMode) error {
	src, err := from.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := to.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}

// NewMachineID is a random machine ID, as systemd would make one.
func NewMachineID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// MachineID returns the machine ID on name's disk, so a box from before
// the host kept them keeps its own, or a new one if the disk has none.
func (s Store) MachineID(name string) (string, error) {
	id := NewMachineID()
	err := s.mounted(name, func(dirs []string) error {
		b, err := os.ReadFile(filepath.Join(dirs[0], "etc/machine-id"))
		if old := strings.TrimSpace(string(b)); err == nil && len(old) == 32 {
			if _, err := hex.DecodeString(old); err == nil {
				id = old
			}
		}
		return nil
	}, mount{device(s.volume(name)), "ro"})
	return id, err
}
