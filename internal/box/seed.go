package box

import (
	"fmt"
	"io"
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

	"github.com/containmentzone/cyacrt/internal/driver"
)

// loginUserLabels name the user `cyac exec` logs in as, in order of
// precedence. exe.dev's label makes exeuntu land as exedev, as it does there.
var loginUserLabels = []string{"cyac.nullvoid.space/login-user", "exe.dev/login-user"}

var validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Seed builds a box's disk from an OCI image: pull, flatten, unpack into a
// scratch dir, drop in a few config files, pick PID 1, and mkfs.ext4 -d it into a sparse
// file of the requested size. It runs once per box; afterwards the disk is
// the box and the image is no longer consulted. progress gets a line per
// step and, while unpacking, every few seconds.
func (s Store) Seed(b *Box, sizeBytes int64, progress func(string)) error {
	progress("resolving " + b.Image)
	img, err := crane.Pull(b.Image, crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		return fmt.Errorf("pull %s: %w", b.Image, err)
	}
	if digest, err := img.Digest(); err == nil {
		layers, _ := img.Layers()
		var compressed int64
		for _, l := range layers {
			n, _ := l.Size()
			compressed += n
		}
		progress(fmt.Sprintf("downloading and unpacking %d layers (%s compressed) of %s", len(layers), mib(compressed), digest))
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return err
	}
	if len(b.Init) == 0 {
		b.Init = append(cfg.Config.Entrypoint, cfg.Config.Cmd...)
	}
	for _, label := range loginUserLabels {
		if u := cfg.Config.Labels[label]; u != "" {
			if !validUser.MatchString(u) {
				return fmt.Errorf("label %s=%q is not a valid user name", label, u)
			}
			b.User = u
			break
		}
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
	counted := &counter{r: layers}
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
		return fmt.Errorf("unpack image: %v: %s", err, out)
	}
	progress("unpacked " + mib(counted.n.Load()))

	if err := writeBoxConfig(scratch, b, s.machine(b)); err != nil {
		return err
	}

	disk := s.DiskPath(b.Name)
	progress("writing a " + mib(sizeBytes) + " ext4 disk")
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
// a systemd-networkd config with the box's static IP, an empty
// machine-id so systemd generates a fresh one on first boot, and the SSH
// keys and units from seedSSH.
// The scratch tree is untrusted image content, so all writes go through
// os.Root, which refuses to follow symlinks out of it.
func writeBoxConfig(scratch string, b *Box, m *driver.Machine) error {
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return err
	}
	defer root.Close()
	addr := m.Addr

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
		if err := writeFile(root, name, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return seedSSH(root, m)
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

func createSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}
