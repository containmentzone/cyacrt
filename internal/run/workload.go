package run

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containmentzone/cyacrt/internal/agent"
	"github.com/containmentzone/cyacrt/internal/box"
	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/render"
)

// Defaults for what a file leaves out.
const (
	defaultCPUs     = 1
	defaultMemoryMB = 1024
	defaultDiskMB   = 10 << 10
)

// vmRuntime is what a box runs under unless its file (Runtime=) or
// CYAC_RUNTIME names another. runc and runsc are for trusted workloads, so
// a box only gets one by asking: a host that loses KVM must not quietly
// move a box out of its VM, or mount on the host a disk the guest wrote.
//
// A service runs under gVisor unless its file says Runtime=runc, and
// never in a VM: its plan builds a netns, not a tap. CYAC_RUNTIME is a
// default for boxes only.
const (
	vmRuntime      = "cloud-hypervisor"
	serviceRuntime = "runsc"
)

// box runs b: its disk (seeded on first start), its port on the network,
// then the runtime, in the foreground.
func (r *Runner) box(ctx context.Context, c *conf.Config, b *conf.Box) error {
	want := cmp.Or(b.Workload.Runtime, os.Getenv("CYAC_RUNTIME"), vmRuntime)
	rt, err := r.runtime(want)
	if err != nil {
		if want == vmRuntime {
			err = fmt.Errorf("%w; set Runtime=runc to run %s as a trusted container instead", err, b.Name)
		}
		return err
	}
	img, err := image(r.store(conf.KindBox), b.Name, b.Workload, orDefault(b.Workload.DiskMB, defaultDiskMB))
	if err != nil {
		return err
	}
	n := c.Networks[b.Network]
	m := machine(r.bare(conf.KindBox, b.Name), b.Workload, img)
	m.Addr = driver.Addr{IP: b.Address, Prefix: n.Prefix().Bits()}
	if n.Gateway != "" {
		// The gateway's resolver listens on the router's address.
		m.Addr.Gateway, m.Addr.DNS = n.Router(), n.Router().String()
	}
	m.Init.DNS, m.Init.Network = m.Addr.DNS, render.Networkd(c, b)
	tap := rt.LinkKind() == driver.Tap
	m.Link.HostIface = render.Port(b)
	if tap {
		m.MAC = render.MAC(b.Address) // the bridge only passes this one
	} else {
		m.Link.Netns = netnsPath(render.BoxNetns(b.Name))
	}
	if err := r.up(conf.KindBox, b.Name, render.Box(c, b, tap)); err != nil {
		return err
	}
	return r.run(ctx, rt, m)
}

// service runs s confined in the netns its plan builds, with the unit's
// credentials (LoadCredential=) at /run/secrets.
func (r *Runner) service(ctx context.Context, c *conf.Config, s *conf.Service) error {
	want := cmp.Or(s.Workload.Runtime, serviceRuntime)
	if want != "runsc" && want != "runc" {
		return fmt.Errorf("%s runs under runsc or runc, not Runtime=%s", s.Name, want)
	}
	rt, err := r.runtime(want)
	if err != nil {
		if want == serviceRuntime {
			err = fmt.Errorf("%w; set Runtime=runc to run %s without gVisor", err, s.Name)
		}
		return err
	}
	img, err := image(r.store(conf.KindService), s.Name, s.Workload, orDefault(s.Workload.DiskMB, defaultDiskMB))
	if err != nil {
		return err
	}
	m := machine(r.bare(conf.KindService, s.Name), s.Workload, img)
	m.Confined = true
	m.Link.Netns = netnsPath(render.ServiceNetns(s.Name))
	// Names resolve through the gateway's resolver, at the gateway end
	// of the service's link.
	gwEnd, _ := s.Link()
	m.Init.DNS = gwEnd.Addr().String()
	if len(s.Secrets) > 0 {
		m.Secrets = os.Getenv("CREDENTIALS_DIRECTORY")
		if m.Secrets == "" {
			return errors.New("no $CREDENTIALS_DIRECTORY: secrets come from the unit's LoadCredential=")
		}
	}
	if err := r.up(conf.KindService, s.Name, render.Service(c, s)); err != nil {
		return err
	}
	return r.run(ctx, rt, m)
}

// run starts m under rt and waits until it exits or ctx ends, in which
// case it stops it. Leftovers are Stop's job (ExecStopPost=).
func (r *Runner) run(ctx context.Context, rt driver.Runtime, m *driver.Machine) error {
	if err := os.MkdirAll(m.RunDir, 0o700); err != nil {
		return err
	}
	// The daemon reads which runtime a workload runs under from here.
	if err := os.WriteFile(filepath.Join(m.RunDir, "runtime"), []byte(rt.Name()), 0o600); err != nil {
		return err
	}
	if err := syscall.Mkfifo(m.ConsoleIn, 0o600); err != nil {
		return err
	}
	var from int64 // where this run's console output starts
	if fi, err := os.Stat(m.ConsoleLog); err == nil {
		from = fi.Size()
	}
	log.Printf("starting %s under %s", m.Name, rt.Name())
	if err := rt.Start(m); err != nil {
		return fmt.Errorf("start %s (%s): %w\n%s", m.Name, rt.Name(), err, consoleSince(m.ConsoleLog, from))
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("stopping %s", m.Name)
			return rt.Stop(m)
		case <-tick.C:
			if !rt.Running(m) {
				log.Printf("%s exited", m.Name)
				return nil
			}
		}
	}
}

// cleanup removes what any runtime may have left of a workload's run.
func (r *Runner) cleanup(kind, name string) {
	m := r.bare(kind, name)
	for _, rt := range r.Runtimes {
		rt.Cleanup(m)
	}
	os.RemoveAll(m.RunDir)
}

// store is where kind's disks, run dirs and logs live: boxes where the
// daemon kept them, services in a tree and dataset of their own, so a
// service can never be handed a box's disk.
func (r *Runner) store(kind string) box.Store {
	if kind == conf.KindService {
		return box.Store{Dir: filepath.Join(r.State, "services"), Dataset: r.Dataset + "/services"}
	}
	return box.Store{Dir: r.State, Dataset: r.Dataset + "/boxes"}
}

// bare is kind/name's machine with only its paths filled in.
func (r *Runner) bare(kind, name string) *driver.Machine {
	st := r.store(kind)
	return &driver.Machine{
		Name: name, Disk: st.DiskPath(name), RunDir: st.RunDir(name),
		ConsoleLog: st.ConsoleLog(name), ConsoleIn: st.ConsoleIn(name),
	}
}

// runtime returns the runtime named want, if this host can run it.
func (r *Runner) runtime(want string) (driver.Runtime, error) {
	for _, rt := range r.Runtimes {
		if rt.Name() != want {
			continue
		}
		if err := rt.Available(); err != nil {
			return nil, fmt.Errorf("%s is unavailable: %w", want, err)
		}
		return rt, nil
	}
	return nil, fmt.Errorf("no runtime %q", want)
}

// image returns what the workload's image says to run, seeding its disk
// first if it has none, or growing it to diskMB if it has. Seeding saves
// the image's defaults next to the disk, so later starts don't need the
// registry; the file only ever changes when the disk is seeded again.
func image(st box.Store, name string, w conf.Workload, diskMB int) (*box.Box, error) {
	meta := imagePath(st, name)
	b, err := loadImage(meta)
	if err == nil {
		return b, st.Grow(name, int64(diskMB)<<20)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, d := range []string{"disks", "run", "logs"} {
		if err := os.MkdirAll(filepath.Join(st.Dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	// A disk without its defaults: a seed that died part way, or a state
	// dir that lost its files. Either way it's not ours to throw away.
	if st.Exists(name) {
		return nil, fmt.Errorf("%s exists but %s doesn't; destroy the zvol to reseed it, or restore the file", st.DiskPath(name), meta)
	}
	if w.Image == "" {
		return nil, fmt.Errorf("no disk yet and no SeedImage= to seed one from")
	}
	b = &box.Box{Name: name, Image: w.Image}
	if err := st.Seed(b, int64(diskMB)<<20, func(line string) { log.Print(line) }); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(meta+".tmp", data, 0o600); err != nil {
		return nil, err
	}
	return b, os.Rename(meta+".tmp", meta)
}

func imagePath(st box.Store, name string) string { return filepath.Join(st.Dir, "disks", name+".json") }

func loadImage(path string) (*box.Box, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b box.Box
	return &b, json.Unmarshal(data, &b)
}

// machine fills in m, a bare machine, for the runtime: the image's
// defaults with the file's settings on top.
func machine(m *driver.Machine, w conf.Workload, img *box.Box) *driver.Machine {
	init := agent.Config{Mode: img.Mode, Args: img.Init, Env: append(img.Env, w.Env...), Cwd: img.Cwd, User: img.RunAs}
	if len(w.Exec) > 0 {
		init.Args, init.Mode = w.Exec, box.ModeFor(w.Exec)
	}
	if w.WorkDir != "" {
		init.Cwd = w.WorkDir
	}
	if w.User != "" {
		init.User = w.User
	}
	m.Init, m.CPUs, m.MemMiB = init, orDefault(w.CPUs, defaultCPUs), orDefault(w.MemoryMB, defaultMemoryMB)
	return m
}

// consoleSince is the last 2 KiB the console logged after offset from:
// usually why a runtime failed to start.
func consoleSince(path string, from int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	from = min(max(from, fi.Size()-2048), fi.Size())
	buf := make([]byte, fi.Size()-from)
	n, _ := f.ReadAt(buf, from)
	return buf[:n]
}

func netnsPath(ns string) string { return "/run/netns/" + ns }

// orDefault returns v, or def if v is zero.
func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
