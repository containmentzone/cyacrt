package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containmentzone/cyacrt/internal/agent"
	"github.com/containmentzone/cyacrt/internal/box"
	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/render"
)

// Defaults for what a file leaves out, as for `cyac new`.
const (
	defaultCPUs     = 1
	defaultMemoryMB = 1024
	defaultDiskMB   = 10 << 10
)

// box runs b: its disk (seeded on first start), its port on the network,
// then the runtime, in the foreground.
func (r *Runner) box(ctx context.Context, c *conf.Config, b *conf.Box) error {
	rt, err := r.runtime(b.Runtime)
	if err != nil {
		return err
	}
	st := r.store(conf.KindBox)
	img, err := image(st, b.Name, b.Workload, orDefault(b.DiskMB, defaultDiskMB))
	if err != nil {
		return err
	}
	n := c.Networks[b.Network]
	m := machine(st, b.Name, b.Workload, img)
	m.Addr = driver.Addr{IP: b.Address, Prefix: n.Prefix().Bits(), DNS: n.Host().String()}
	if n.Gateway != "" {
		m.Addr.Gateway = n.Router()
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

// service runs s under runc in the netns its plan builds, with the unit's
// credentials (LoadCredential=) at /run/secrets.
func (r *Runner) service(ctx context.Context, c *conf.Config, s *conf.Service) error {
	rt, err := r.runtime("runc")
	if err != nil {
		return err
	}
	st := r.store(conf.KindService)
	img, err := image(st, s.Name, s.Workload, defaultDiskMB)
	if err != nil {
		return err
	}
	m := machine(st, s.Name, s.Workload, img)
	m.Confined = true
	m.Link.Netns = netnsPath(render.ServiceNetns(s.Name))
	// Names resolve through the gateway's resolver if it names one, and
	// otherwise through cyac dns on the first network that attaches s.
	m.Init.DNS = c.Gateways[s.Gateway].DNS
	if nets := c.AttachedBy(s.Name); m.Init.DNS == "" && len(nets) > 0 {
		m.Init.DNS = c.Networks[nets[0]].Host().String()
	}
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
	st := r.store(kind)
	m := &driver.Machine{Name: name, Disk: st.DiskPath(name), RunDir: st.RunDir(name)}
	for _, rt := range r.Runtimes {
		rt.Cleanup(m)
	}
	os.RemoveAll(m.RunDir)
}

// store is where kind's disks, run dirs and logs live: boxes where the
// daemon kept them, services in a tree of their own, so a service can
// never be handed a box's disk.
func (r *Runner) store(kind string) box.Store {
	if kind == conf.KindService {
		return box.Store{Dir: filepath.Join(r.State, "services")}
	}
	return box.Store{Dir: r.State}
}

// runtime returns the runtime named want, else the one CYAC_RUNTIME
// names, else the first available.
func (r *Runner) runtime(want string) (driver.Runtime, error) {
	if want == "" {
		want = os.Getenv("CYAC_RUNTIME")
	}
	var why []string
	for _, rt := range r.Runtimes {
		if want != "" && rt.Name() != want {
			continue
		}
		err := rt.Available()
		if err == nil {
			return rt, nil
		}
		why = append(why, rt.Name()+": "+err.Error())
	}
	if len(why) == 0 {
		return nil, fmt.Errorf("no runtime %q", want)
	}
	return nil, fmt.Errorf("no usable runtime (%s)", strings.Join(why, "; "))
}

// image returns what the workload's image says to run, seeding its disk
// first if it has none. Seeding saves the image's defaults next to the
// disk, so later starts don't need the registry; the file only ever
// changes when the disk is seeded again.
func image(st box.Store, name string, w conf.Workload, diskMB int) (*box.Box, error) {
	meta := filepath.Join(st.Dir, "disks", name+".json")
	data, err := os.ReadFile(meta)
	if err == nil {
		var b box.Box
		return &b, json.Unmarshal(data, &b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, d := range []string{"disks", "run", "logs"} {
		if err := os.MkdirAll(filepath.Join(st.Dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	// A disk without its defaults is a seed that died part way.
	os.Remove(st.DiskPath(name))
	b := &box.Box{Name: name, Image: w.Image}
	if err := st.Seed(b, int64(diskMB)<<20, func(line string) { log.Print(line) }); err != nil {
		return nil, err
	}
	if data, err = json.MarshalIndent(b, "", "  "); err != nil {
		return nil, err
	}
	if err := os.WriteFile(meta+".tmp", data, 0o600); err != nil {
		return nil, err
	}
	return b, os.Rename(meta+".tmp", meta)
}

// machine is the runtime's view of a workload: the image's defaults with
// the file's settings on top.
func machine(st box.Store, name string, w conf.Workload, img *box.Box) *driver.Machine {
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
	return &driver.Machine{
		Name: name, Disk: st.DiskPath(name), RunDir: st.RunDir(name),
		ConsoleLog: st.ConsoleLog(name), ConsoleIn: st.ConsoleIn(name),
		Init: init, CPUs: orDefault(w.CPUs, defaultCPUs), MemMiB: orDefault(w.MemoryMB, defaultMemoryMB),
	}
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
