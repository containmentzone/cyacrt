// Package driver defines cyac's runtime seam: a Runtime boots a workload's
// disk. Runtimes live in internal/runtime/*; package run drives them,
// after the workload's render plan has built its link.
package driver

import (
	"net/netip"
	"os"
	"os/exec"

	"github.com/containmentzone/cyacrt/internal/agent"
)

// Machine is everything a driver needs to know about one box.
type Machine struct {
	Name   string
	Disk   string // path to the box's ext4 disk
	RunDir string // private scratch dir; exists only while running
	// The init's console: runtimes append its output to ConsoleLog, which
	// outlives the run, and feed it input from the FIFO at ConsoleIn.
	ConsoleLog string
	ConsoleIn  string
	Init       agent.Config // what runs as PID 1
	CPUs       int
	MemMiB     int
	Addr       Addr // a VM's address, for its kernel command line
	Link       Link // built by the render plan before Runtime.Start
	// MAC is eth0's address when the network pins one (VMs only: a
	// container's netns already has it). Empty lets the runtime pick.
	MAC string
	// Secrets is a host directory a container sees read-only at
	// /run/secrets, or empty (runc only).
	Secrets string
}

// Addr is a box's static network config. cyac writes it into the box on
// every start (see agent.WriteNetConfig).
type Addr struct {
	IP      netip.Addr
	Prefix  int
	Gateway netip.Addr
	DNS     string
}

// LinkKind is the kind of attachment a runtime consumes.
type LinkKind int

const (
	Tap   LinkKind = iota // a host tap device handed to a VMM
	Netns                 // a network namespace with eth0 configured
)

// Link is what a box's render plan built for it.
type Link struct {
	HostIface string // tap device, or the host end of a veth
	Netns     string // netns path; empty for Tap
}

// OpenConsole opens m's console for a runtime to hand to the init: the log
// for appending output, and the input FIFO read-write, so it never sees EOF
// while nobody is attached and never blocks on open.
func OpenConsole(m *Machine) (log, in *os.File, err error) {
	log, err = os.OpenFile(m.ConsoleLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	in, err = os.OpenFile(m.ConsoleIn, os.O_RDWR, 0)
	if err != nil {
		log.Close()
		return nil, nil, err
	}
	return log, in, nil
}

// Runtime boots and stops a box's disk.
type Runtime interface {
	// Name identifies the runtime ("cloud-hypervisor", "runc"); it is recorded
	// while a box runs and accepted by CYAC_RUNTIME.
	Name() string
	// Available reports why this host can't use the runtime, or nil.
	Available() error
	// LinkKind says which attachment Start expects in m.Link.
	LinkKind() LinkKind
	Start(m *Machine) error
	// Stop asks the box's init for a clean shutdown, then forces it.
	Stop(m *Machine) error
	Running(m *Machine) bool
	// Cleanup removes leftovers of a stopped or crashed run. Idempotent.
	Cleanup(m *Machine)
	// Grow makes an already extended disk file visible at its new size.
	Grow(m *Machine) error
	// Command returns a command that runs args inside the box when started
	// with a terminal on its stdio (the daemon attaches a pty).
	Command(m *Machine, args []string) (*exec.Cmd, error)
}
