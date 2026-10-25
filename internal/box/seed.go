package box

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/containmentzone/cyacrt/internal/sh"
)

// loginUserLabels name the user `cyac shell` logs in as, in order of
// precedence. exe.dev's label makes exeuntu land as exedev, as it does there.
var loginUserLabels = []string{"cyac.nullvoid.space/login-user", "exe.dev/login-user"}

var validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Seed builds a box's disk from an OCI image. The image's filesystem is
// unpacked once per image digest into a base disk (see base); each box
// gets a copy of it, grown to the requested size, with the box's own few
// files written in. After that the disk is the box and neither the image
// nor the base is consulted again. progress gets a line per step and,
// while unpacking, every few seconds.
//
// b.Mode asks for a mode ("init", "command" or "pause"); empty picks one
// from the image (see pickMode). Seed replaces it with the mode picked.
func (s Store) Seed(b *Box, sizeBytes int64, progress func(string)) error {
	progress("resolving " + b.Image)
	img, err := crane.Pull(b.Image, crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		return fmt.Errorf("pull %s: %w", b.Image, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return err
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return err
	}
	b.Digest = digest.String()
	if len(b.Init) == 0 {
		b.Init = append(cfg.Config.Entrypoint, cfg.Config.Cmd...)
	}
	b.Env, b.Cwd, b.RunAs = cfg.Config.Env, cfg.Config.WorkingDir, cfg.Config.User
	for _, label := range loginUserLabels {
		if u := cfg.Config.Labels[label]; u != "" {
			if !validUser.MatchString(u) {
				return fmt.Errorf("label %s=%q is not a valid user name", label, u)
			}
			b.User = u
			break
		}
	}

	base, err := s.base(img, digest, progress)
	if err != nil {
		return err
	}
	disk := s.DiskPath(b.Name)
	if _, err := os.Lstat(disk); err == nil {
		return fmt.Errorf("%s already exists", disk)
	}
	progress("copying the base disk and growing it to " + mib(sizeBytes))
	if err := clone(base, disk, sizeBytes); err != nil {
		os.Remove(disk)
		return err
	}
	if err := s.configure(disk, b); err != nil {
		os.Remove(disk)
		return err
	}
	return nil
}

// base returns the image's base disk, building it if this is the first box
// of that digest: pull, flatten, unpack into a scratch dir and mkfs.ext4 -d
// it into a sparse file just big enough to hold it. It holds the image and
// nothing of any box, so every box of the image can start from it.
func (s Store) base(img v1.Image, digest v1.Hash, progress func(string)) (string, error) {
	path := s.BasePath(digest)
	if _, err := os.Stat(path); err == nil {
		progress("using the base disk unpacked earlier for " + digest.String())
		return path, nil
	}
	layers, err := img.Layers()
	if err != nil {
		return "", err
	}
	var compressed int64
	for _, l := range layers {
		n, _ := l.Size()
		compressed += n
	}
	progress(fmt.Sprintf("downloading and unpacking %d layers (%s compressed) of %s", len(layers), mib(compressed), digest))

	scratch, err := os.MkdirTemp(filepath.Join(s.Dir, "bases"), digest.Hex+".seed-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)

	// mutate.Extract yields the flattened filesystem with whiteouts applied.
	// GNU tar defers symlink creation, so entries can't write outside scratch.
	flat := mutate.Extract(img)
	defer flat.Close()
	counted := &counter{r: flat}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				progress("unpacked " + mib(counted.n.Load()))
			case <-done:
				return
			}
		}
	}()
	untar := exec.Command("tar", "-x", "-p", "--numeric-owner", "--xattrs", "--xattrs-include=*", "-C", scratch)
	untar.Stdin = counted
	out, err := untar.CombinedOutput()
	close(done)
	if err != nil {
		return "", fmt.Errorf("unpack image: %v: %s", err, out)
	}
	progress("unpacked " + mib(counted.n.Load()))

	size, err := baseSize(scratch)
	if err != nil {
		return "", err
	}
	progress("writing a " + mib(size) + " base disk")
	// Built under another name, so a crash never leaves a half-made base.
	tmp := path + ".tmp"
	os.Remove(tmp)
	defer os.Remove(tmp)
	if err := createSparse(tmp, size); err != nil {
		return "", err
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", "-L", "cyac-root", "-d", scratch, tmp).CombinedOutput(); err != nil {
		return "", fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	// On disk before anyone clones it: ZFS only shares blocks it has
	// written out, and copies the rest.
	if err := syncFile(tmp); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// baseSize is a disk size that fits the tree at dir, with a quarter to
// spare and enough inodes at mkfs.ext4's default of one per 16 KiB.
// The base keeps that density as it grows, so a box can hold as many
// small files as one made at full size.
func baseSize(dir string) (int64, error) {
	var bytes, files int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		files++
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			bytes += fi.Size()
		}
		return nil
	})
	size := max(bytes+files*4096, files*16384) * 5 / 4
	return (size>>20 + 256) << 20, err
}

// clone copies base to disk, sharing blocks where the filesystem can
// (btrfs, XFS, ZFS with block cloning), and grows it to size.
func clone(base, disk string, size int64) error {
	fi, err := os.Stat(base)
	if err != nil {
		return err
	}
	if size < fi.Size() {
		return fmt.Errorf("this image needs a disk of at least %s", mib(fi.Size()))
	}
	if err := sh.Run("cp", "--reflink=auto", "--sparse=always", base, disk); err != nil {
		return err
	}
	if err := os.Truncate(disk, size); err != nil {
		return err
	}
	return sh.Run("resize2fs", disk)
}

// configure loop-mounts a box's fresh disk and writes its files.
func (s Store) configure(disk string, b *Box) error {
	mnt, err := os.MkdirTemp(filepath.Join(s.Dir, "disks"), b.Name+".mnt-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)
	if err := sh.Run("mount", "-o", "loop,nosuid,nodev,noexec", disk, mnt); err != nil {
		return err
	}
	err = writeBoxConfig(mnt, b)
	if uerr := sh.Run("umount", mnt); err == nil {
		err = uerr
	}
	return err
}

// writeBoxConfig picks the box's mode and drops the only files cyac adds
// to a box's disk: hostname, hosts (so `hostname -f` resolves), and an empty
// machine-id so systemd generates a fresh one on first boot. The network
// config is written on every start instead (agent.WriteNetConfig).
// The tree at dir is untrusted image content, so all writes go through
// os.Root, which refuses to follow symlinks out of it.
func writeBoxConfig(dir string, b *Box) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := pickMode(root, b); err != nil {
		return err
	}

	files := map[string]string{
		"etc/hostname":   b.Name + "\n",
		"etc/hosts":      "127.0.0.1 localhost\n127.0.1.1 " + b.Name + "\n::1 localhost ip6-localhost ip6-loopback\n",
		"etc/machine-id": "",
	}
	if err := root.Mkdir("etc", 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	for name, content := range files {
		if err := writeFile(root, name, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// pickMode settles what runs b.Init, unless b.Mode already asks: an init
// (named init or systemd, like exeuntu's /usr/local/bin/init) becomes PID
// 1, any other command runs under cyac-init, like a container, and an
// image with neither a command nor /sbin/init gets pause mode.
func pickMode(root *os.Root, b *Box) error {
	want := b.Mode
	b.Mode = ""
	switch {
	case want == "pause":
		b.Mode, b.Init = "pause", nil
		return nil
	case len(b.Init) == 0 && want == "command":
		return errors.New("the image has no command (pass --init to give one)")
	case len(b.Init) == 0:
		if _, err := root.Lstat("sbin/init"); err != nil {
			if want == "init" {
				return errors.New("the image has no command or /sbin/init (pass --init to give one)")
			}
			b.Mode = "pause"
			return nil
		}
		b.Init = []string{"/sbin/init"}
	}
	if base := filepath.Base(b.Init[0]); want == "command" || want == "" && base != "init" && base != "systemd" {
		b.Mode = "command"
		return nil
	}
	if _, err := root.Lstat(strings.TrimPrefix(b.Init[0], "/")); err != nil {
		return fmt.Errorf("init %s not found in image (pass --init to choose one): %w", b.Init[0], err)
	}
	return nil
}

// writeFile replaces name in root, without following a symlink there.
func writeFile(root *os.Root, name string, content []byte, mode os.FileMode) error {
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// counter counts the bytes read through it.
type counter struct {
	r io.Reader
	n atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func mib(n int64) string { return fmt.Sprintf("%d MiB", n>>20) }

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func createSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}
