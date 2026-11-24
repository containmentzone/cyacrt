package box

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/containmentzone/cyacrt/internal/agent"
)

// loginUserLabels name the user `cyac shell` logs in as, in order of
// precedence. exe.dev's label makes exeuntu land as exedev, as it does there.
var loginUserLabels = []string{"cyac.nullvoid.space/login-user", "exe.dev/login-user"}

var validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// exposeLabel followed by <name> gives a port `cyac expose` serves by
// default, as <name>.
const exposeLabel = "cyac.nullvoid.space/expose."

// exposes reads the default exposes from an image's labels, by name.
func exposes(labels map[string]string) (map[string]int, error) {
	var ports map[string]int
	for label, v := range labels {
		name, ok := strings.CutPrefix(label, exposeLabel)
		if !ok {
			continue
		}
		port, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("label %s=%q is not a port", label, v)
		}
		if ports == nil {
			ports = map[string]int{}
		}
		ports[name] = port
	}
	return ports, nil
}

// Seed builds a box's disk from an OCI image: pull, flatten, unpack into a
// scratch dir, drop in a few config files, pick PID 1, and mkfs.ext4 -d it onto a sparse
// zvol of the requested size. It runs once per box; afterwards the disk is
// the box and the image is no longer consulted. progress gets a line per
// step and, while unpacking, every few seconds. Seed fills in the rest of
// b from the image, including the mode it picks (see pickMode).
func (s Store) Seed(b *Box, sizeBytes int64, progress func(string)) error {
	progress("resolving " + b.Image)
	img, err := pull(b.Image)
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
	b.Init = append(cfg.Config.Entrypoint, cfg.Config.Cmd...)
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
	if b.Expose, err = exposes(cfg.Config.Labels); err != nil {
		return err
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

	if err := writeBoxConfig(scratch, b); err != nil {
		return err
	}

	progress("writing a " + mib(sizeBytes) + " ext4 disk to " + s.volume(b.Name))
	if err := s.createVolume(b.Name, sizeBytes); err != nil {
		return err
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", "-L", "cyac-root", "-d", scratch, s.DiskPath(b.Name)).CombinedOutput(); err != nil {
		s.Destroy(b.Name)
		return fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	return nil
}

// pull fetches ref from its registry, or, if ref is an absolute path,
// reads it from that file: an uncompressed `docker save` tarball, such as
// the flake's dockerTools images with compressor = "none".
func pull(ref string) (v1.Image, error) {
	if filepath.IsAbs(ref) {
		return crane.Load(ref)
	}
	return crane.Pull(ref, crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
}

// writeBoxConfig picks the box's mode and drops the only files cyac adds
// to an image: hostname, hosts (so `hostname -f` resolves), and an empty
// machine-id so systemd generates a fresh one on first boot. The network
// config is written on every start instead (agent.WriteNetConfig).
// The scratch tree is untrusted image content, so all writes go through
// os.Root, which refuses to follow symlinks out of it.
func writeBoxConfig(scratch string, b *Box) error {
	root, err := os.OpenRoot(scratch)
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
		if err := agent.ReplaceFile(root, name, content); err != nil {
			return err
		}
	}
	return nil
}

// pickMode settles what runs b.Init: an init (named init or systemd, like
// exeuntu's /usr/local/bin/init) becomes PID 1, any other command runs
// under cyac-init, like a container, and an image with neither a command
// nor /sbin/init gets pause mode.
func pickMode(root *os.Root, b *Box) error {
	if len(b.Init) == 0 {
		if _, err := root.Lstat("sbin/init"); err != nil {
			b.Mode = "pause"
			return nil
		}
		b.Init = []string{"/sbin/init"}
	}
	if b.Mode = ModeFor(b.Init); b.Mode == "command" {
		return nil
	}
	if _, err := root.Lstat(strings.TrimPrefix(b.Init[0], "/")); err != nil {
		return fmt.Errorf("init %s not found in image: %w", b.Init[0], err)
	}
	return nil
}

// ModeFor is the mode a command gets when nothing asks for one: an init
// (named init or systemd) becomes PID 1, anything else runs under
// cyac-init like a container.
func ModeFor(init []string) string {
	if base := filepath.Base(init[0]); base == "init" || base == "systemd" {
		return ""
	}
	return "command"
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
