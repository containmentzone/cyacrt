package box

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/containmentzone/cyacrt/internal/driver"
)

// Seed builds a box's disk from an OCI image: pull, flatten, unpack into a
// scratch dir, drop in a few config files, pick PID 1, and mkfs.ext4 -d it into a sparse
// file of the requested size. It runs once per box; afterwards the disk is
// the box and the image is no longer consulted.
func (s Store) Seed(b *Box, sizeBytes int64) error {
	img, err := crane.Pull(b.Image, crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		return fmt.Errorf("pull %s: %w", b.Image, err)
	}
	if len(b.Init) == 0 {
		cfg, err := img.ConfigFile()
		if err != nil {
			return err
		}
		b.Init = append(cfg.Config.Entrypoint, cfg.Config.Cmd...)
	}
	if len(b.Init) == 0 {
		b.Init = []string{"/sbin/init"}
	}
	for _, arg := range b.Init {
		// The VM tier passes these on the kernel command line.
		if arg == "" || strings.ContainsAny(arg, " \t\n\"") {
			return fmt.Errorf("init argument %q can't contain whitespace or quotes", arg)
		}
	}

	scratch, err := os.MkdirTemp(filepath.Join(s.Dir, "disks"), b.Name+".seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	// mutate.Extract yields the flattened filesystem with whiteouts applied.
	// GNU tar defers symlink creation, so entries can't write outside scratch.
	layers := mutate.Extract(img)
	defer layers.Close()
	untar := exec.Command("tar", "-x", "-p", "--numeric-owner", "--xattrs", "--xattrs-include=*", "-C", scratch)
	untar.Stdin = layers
	if out, err := untar.CombinedOutput(); err != nil {
		return fmt.Errorf("unpack image: %v: %s", err, out)
	}

	if err := writeBoxConfig(scratch, b, s.Net.Addr(b.ID)); err != nil {
		return err
	}

	disk := s.DiskPath(b.Name)
	if err := createSparse(disk, sizeBytes); err != nil {
		return err
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", "-L", "cyac-root", "-d", scratch, disk).CombinedOutput(); err != nil {
		os.Remove(disk)
		return fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	return nil
}

// writeBoxConfig drops the only files cyac adds to an image: hostname,
// a systemd-networkd config with the box's static IP, and an empty
// machine-id so systemd generates a fresh one on first boot.
// The scratch tree is untrusted image content, so all writes go through
// os.Root, which refuses to follow symlinks out of it.
func writeBoxConfig(scratch string, b *Box, addr driver.Addr) error {
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return err
	}
	defer root.Close()

	if _, err := root.Lstat(strings.TrimPrefix(b.Init[0], "/")); err != nil {
		return fmt.Errorf("init %s not found in image (pass --init to choose one): %w", b.Init[0], err)
	}

	network := fmt.Sprintf("[Match]\nName=eth0\n\n[Network]\nAddress=%s/%d\nGateway=%s\nDNS=%s\n",
		addr.IP, addr.Prefix, addr.Gateway, addr.DNS)
	files := map[string]string{
		"etc/hostname":                        b.Name + "\n",
		"etc/machine-id":                      "",
		"etc/systemd/network/10-cyac.network": network,
	}
	for _, dir := range []string{"etc", "etc/systemd", "etc/systemd/network"} {
		if err := root.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	for name, content := range files {
		if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = f.WriteString(content)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func createSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}
