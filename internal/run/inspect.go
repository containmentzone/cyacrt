package run

import (
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
	m := r.bare(kind, name)
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
	b, err := loadImage(imagePath(r.store(kind), name))
	if err != nil {
		return nil
	}
	return b
}

// Console returns the paths of kind/name's console log and input FIFO.
func (r *Runner) Console(kind, name string) (log, in string) {
	m := r.bare(kind, name)
	return m.ConsoleLog, m.ConsoleIn
}
