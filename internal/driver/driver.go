// Package driver defines the two seams of cyac: a Runtime boots a box's
// disk, and a Network gives the box a link and an address. Drivers live in
// internal/runtime/* and internal/network/*; package box drives them.
package driver

import (
	"net/netip"
	"os/exec"
)

// Machine is everything a driver needs to know about one box.
type Machine struct {
	Name   string
	ID     int    // small integer, unique per host
	Disk   string // path to the box's ext4 disk
	RunDir string // private scratch dir; exists only while running. Runtimes
	//                 write the init's output to RunDir/console.log.
	Init   []string // PID 1
	CPUs   int
	MemMiB int
	Addr   Addr // from Network.Addr
	Link   Link // set by Network.Attach before Runtime.Start
}

// Addr is a box's static network config. It is also written into the disk
// at seed time, so it must not change for the life of the box.
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

// Link is what Network.Attach created for a box.
type Link struct {
	HostIface string // tap device, or the host end of a veth
	Netns     string // netns path; empty for Tap
}

// Runtime boots and stops a box's disk.
type Runtime interface {
	// Name identifies the runtime ("firecracker", "runc"); it is recorded
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

// Network gives boxes links and addresses.
type Network interface {
	// Setup prepares host-wide state (bridges, firewall). Idempotent.
	Setup() error
	// Addr returns the static address for box id.
	Addr(id int) Addr
	// Attach creates m's link of the given kind and sets m.Link.
	Attach(m *Machine, kind LinkKind) error
	// Detach removes whatever Attach created. Idempotent.
	Detach(m *Machine)
}
