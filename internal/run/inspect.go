package run

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/containmentzone/cyacrt/internal/box"
	"github.com/containmentzone/cyacrt/internal/driver"
)

// What the daemon needs to see of a workload that a unit runs: whether it
// is up and under which runtime, and where its console is. None of this
// changes anything.

// Running returns the runtime kind/name runs under and the machine to
// reach it through, or nil if it isn't running.
func (r *Runner) Running(kind, name string) (driver.Runtime, *driver.Machine) {
	st := r.store(kind)
	m := &driver.Machine{
		Name: name, Disk: st.DiskPath(name), RunDir: st.RunDir(name),
		ConsoleLog: st.ConsoleLog(name), ConsoleIn: st.ConsoleIn(name),
	}
	rtName, err := os.ReadFile(filepath.Join(m.RunDir, "runtime"))
	if err != nil {
		return nil, nil
	}
	for _, rt := range r.Runtimes {
		if rt.Name() == string(rtName) && rt.Running(m) {
			return rt, m
		}
	}
	return nil, nil
}

// Image returns the image defaults saved when kind/name's disk was
// seeded, or nil before the first start.
func (r *Runner) Image(kind, name string) *box.Box {
	data, err := os.ReadFile(filepath.Join(r.store(kind).Dir, "disks", name+".json"))
	if err != nil {
		return nil
	}
	var b box.Box
	if json.Unmarshal(data, &b) != nil {
		return nil
	}
	return &b
}

// Console returns the paths of kind/name's console log and input FIFO.
func (r *Runner) Console(kind, name string) (log, in string) {
	st := r.store(kind)
	return st.ConsoleLog(name), st.ConsoleIn(name)
}
