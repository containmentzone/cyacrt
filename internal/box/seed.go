package box

import (
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/containmentzone/cyacrt/internal/agent"
	"github.com/containmentzone/cyacrt/internal/sh"
)

// loginUserLabels name the user `cyac shell` logs in as, in order of
// precedence. exe.dev's label makes exeuntu land as exedev, as it does there.
var loginUserLabels = []string{"cyac.nullvoid.space/login-user", "exe.dev/login-user"}

var validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// exposeLabel followed by <name> gives a port `cyac expose` serves by
// default, as <name>.
const exposeLabel = "cyac.nullvoid.space/expose."

// rootLabel set to "keep" marks an image whose disks are never reseeded
// when a workload moves to another image (see Defaults.KeepRoot).
const rootLabel = "cyac.nullvoid.space/root"

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

// Seed gives a box its disk from an OCI image. The image is resolved to
// its ID and unpacked once per ID (see Images); the box's disk is a clone
// of that, grown to sizeBytes, and the box takes the image's defaults.
// Only a box that asks for a smaller disk than the image's gets one of
// its own, unpacked again, since a clone can't shrink. Either way it runs
// once per box; afterwards the disk is the box and the image is no longer
// consulted. progress gets a line per step and, while unpacking, every
// few seconds.
func (s Store) Seed(b *Box, sizeBytes int64, progress func(string)) error {
	return s.seed(b, s.volume(b.Name), sizeBytes, progress)
}

// seed makes vol, a new disk for b, as Seed describes.
func (s Store) seed(b *Box, vol string, sizeBytes int64, progress func(string)) error {
	progress("resolving " + b.Image)
	img, err := pull(b.Image)
	if err != nil {
		return fmt.Errorf("pull %s: %w", b.Image, err)
	}
	id, err := img.ConfigName()
	if err != nil {
		return err
	}
	b.ImageID = id.String()
	unlock, err := s.Images.lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	rec, err := s.Images.get(img, id, progress)
	if err != nil {
		return err
	}
	b.Defaults = rec.Defaults

	imageSize, err := volsize(s.Images.volume(id))
	if err != nil {
		return err
	}
	if sizeBytes >= imageSize {
		progress("cloning " + s.Images.snapshot(id) + " to " + vol)
		if err := cloneVolume(s.Images.snapshot(id), vol); err != nil {
			return err
		}
		if err := grow(vol, sizeBytes); err != nil {
			return err
		}
	} else {
		progress(fmt.Sprintf("%s is smaller than the image's %s, unpacking it again", mib(sizeBytes), mib(imageSize)))
		if _, err := unpack(img, filepath.Join(s.Dir, "disks"), vol, sizeBytes, progress); err != nil {
			return err
		}
	}
	return s.Images.used(id, rec)
}

// unpack writes img's filesystem to vol, a new zvol of size bytes, or of
// the size the tree needs if size is 0: pull, flatten, unpack into a
// scratch dir under tmp, drop in a few config files, pick PID 1, and
// mkfs.ext4 -d it onto the zvol. It returns the image's defaults.
func unpack(img v1.Image, tmp, vol string, size int64, progress func(string)) (Defaults, error) {
	var d Defaults
	cfg, err := img.ConfigFile()
	if err != nil {
		return d, err
	}
	d.Init = append(cfg.Config.Entrypoint, cfg.Config.Cmd...)
	d.Env, d.Cwd, d.RunAs = cfg.Config.Env, cfg.Config.WorkingDir, cfg.Config.User
	for _, label := range loginUserLabels {
		if u := cfg.Config.Labels[label]; u != "" {
			if !validUser.MatchString(u) {
				return d, fmt.Errorf("label %s=%q is not a valid user name", label, u)
			}
			d.User = u
			break
		}
	}
	if d.Expose, err = exposes(cfg.Config.Labels); err != nil {
		return d, err
	}
	if d.Volumes, d.KeepRoot, err = imageVolumes(cfg); err != nil {
		return d, err
	}
	if layers, err := img.Layers(); err == nil {
		var compressed int64
		for _, l := range layers {
			n, _ := l.Size()
			compressed += n
		}
		progress(fmt.Sprintf("downloading and unpacking %d layers (%s compressed)", len(layers), mib(compressed)))
	}

	scratch, err := os.MkdirTemp(tmp, filepath.Base(vol)+".seed-")
	if err != nil {
		return d, err
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
		return d, fmt.Errorf("unpack image: %v: %s", err, out)
	}
	progress("unpacked " + mib(counted.n.Load()))

	if err := prepare(scratch, &d); err != nil {
		return d, err
	}
	if size == 0 {
		if size, err = treeSize(scratch); err != nil {
			return d, err
		}
	}

	progress("writing a " + mib(size) + " ext4 disk to " + vol)
	if err := createVolume(vol, size); err != nil {
		return d, err
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", "-L", "cyac-root", "-d", scratch, device(vol)).CombinedOutput(); err != nil {
		sh.Run("zfs", "destroy", vol)
		return d, fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	return d, nil
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

// prepare picks the image's mode and readies the tree for any workload:
// an empty machine-id, so systemd generates a fresh one on first boot,
// and no hostname or hosts, which a workload gets on its first start
// (agent.WriteNetConfig), as it gets its network config on every start.
// The scratch tree is untrusted image content, so all writes go through
// os.Root, which refuses to follow symlinks out of it.
func prepare(scratch string, d *Defaults) error {
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := pickMode(root, d); err != nil {
		return err
	}
	if err := root.Mkdir("etc", 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	for _, name := range []string{"etc/hostname", "etc/hosts"} {
		if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return agent.ReplaceFile(root, "etc/machine-id", "")
}

// pickMode settles what runs b.Init: an init (named init or systemd, like
// exeuntu's /usr/local/bin/init) becomes PID 1, any other command runs
// under cyac-init, like a container, and an image with neither a command
// nor /sbin/init gets pause mode.
func pickMode(root *os.Root, b *Defaults) error {
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

// imageVolumes reads an image's VOLUMEs and whether its root label says
// to keep the root (see Defaults).
func imageVolumes(cfg *v1.ConfigFile) ([]string, bool, error) {
	vols := slices.Sorted(maps.Keys(cfg.Config.Volumes))
	switch v := cfg.Config.Labels[rootLabel]; v {
	case "keep":
		return vols, true, nil
	case "", "replace":
		return vols, false, nil
	default:
		return nil, false, fmt.Errorf("label %s=%q: want keep or replace", rootLabel, v)
	}
}
