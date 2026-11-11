// Package run carries out the plans package render makes. It is the only
// networking code that touches the kernel, and it does so only by running
// a plan's commands, so what it does is always what the plan says.
//
// Start records the plan's Down steps before it runs Up, and Stop runs the
// recorded steps, not a fresh render. A unit whose file was edited or
// removed while it ran still removes exactly what it created.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/render"
)

// Dir holds one record per running object. It lives on /run, so records
// go away at reboot together with the kernel objects they describe. It is
// not the daemon's /run/cyac, which systemd empties when the daemon stops.
const Dir = "/run/cyac-run"

// Exec runs one step.
type Exec func(render.Step) error

// Command is the Exec that runs steps for real, logging each one to log.
func Command(log io.Writer) Exec {
	return func(s render.Step) error {
		fmt.Fprintln(log, "+", strings.Join(s.Args, " "))
		cmd := exec.Command(s.Args[0], s.Args[1:]...)
		if s.Stdin != "" {
			cmd.Stdin = strings.NewReader(s.Stdin)
		}
		cmd.Stdout, cmd.Stderr = log, log
		return cmd.Run()
	}
}

// Runner starts and stops objects.
type Runner struct {
	Conf string // conf dir to render from
	Dir  string // where records go
	Exec Exec
	// Workloads: where disks, run dirs and console logs live (see
	// package box), and the runtimes a box may use, in order of
	// preference. Services always use runc.
	State    string
	Runtimes []driver.Runtime
}

// Start brings kind/name up. It first clears whatever an earlier run left
// behind, so a start after a crash doesn't trip over its own objects, and
// it undoes a half-finished Up before returning its error. A gateway or
// network is done once its plan ran; a box or service runs in the
// foreground until it exits or ctx ends.
func (r *Runner) Start(ctx context.Context, kind, name string) error {
	c, probs, err := conf.Load(r.Conf)
	if err != nil {
		return err
	}
	for _, p := range probs {
		if p.Kind == kind && p.Name == name {
			return p.Err
		}
	}
	missing := fmt.Errorf("no %s %s in the conf", kind, name)
	switch kind {
	case conf.KindGateway:
		if g := c.Gateways[name]; g != nil {
			return r.up(kind, name, render.Gateway(g))
		}
	case conf.KindNetwork:
		if n := c.Networks[name]; n != nil {
			return r.up(kind, name, render.Network(n))
		}
	case conf.KindBox:
		if b := c.Boxes[name]; b != nil {
			return r.box(ctx, c, b)
		}
	case conf.KindService:
		if s := c.Services[name]; s != nil {
			return r.service(ctx, c, s)
		}
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return missing
}

// up clears an earlier run of kind/name, records p's Down, and runs its Up.
func (r *Runner) up(kind, name string, p render.Plan) error {
	if err := r.Stop(kind, name); err != nil {
		return err
	}
	if err := r.record(kind, name, p.Down); err != nil {
		return err
	}
	for _, s := range p.Up {
		if err := r.Exec(s); err != nil {
			r.Stop(kind, name)
			return fmt.Errorf("%s: %w", strings.Join(s.Args, " "), err)
		}
	}
	return nil
}

// Stop runs the Down steps Start recorded, all of them whatever fails, and
// then drops the record. Without a record there is nothing to do. For a
// workload it first removes what its runtime left (see cleanup).
func (r *Runner) Stop(kind, name string) error {
	if !conf.ValidName(name) {
		return fmt.Errorf("invalid name %q", name)
	}
	if kind == conf.KindBox || kind == conf.KindService {
		r.cleanup(kind, name)
	}
	path := r.path(kind, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var down []render.Step
	if err := json.Unmarshal(data, &down); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, s := range down {
		r.Exec(s) // Down tolerates objects that aren't there
	}
	return os.Remove(path)
}

func (r *Runner) path(kind, name string) string {
	return filepath.Join(r.Dir, name+"."+kind+".json")
}

func (r *Runner) record(kind, name string, down []render.Step) error {
	data, err := json.Marshal(down)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	tmp := r.path(kind, name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path(kind, name))
}
