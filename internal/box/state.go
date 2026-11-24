// Package box seeds a workload's disk from an OCI image (Seed) and knows
// where a workload's files live (Store). Which workloads exist is the
// conf's business, and running them is package run's.
package box

import "path/filepath"

// Box is what seeding learned from the image, saved next to the disk so
// later starts don't need the registry.
type Box struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// Init is the image's ENTRYPOINT+CMD, e.g. /usr/local/bin/init for
	// exeuntu, or /sbin/init if the image has neither. Mode says what runs
	// it (see agent.Config.Mode): "" makes it PID 1, "command" runs it
	// under cyac-init, and "pause" runs nothing.
	Init []string `json:"init"`
	Mode string   `json:"mode,omitempty"`
	// The image's environment, working directory and USER, for commands.
	Env   []string `json:"env,omitempty"`
	Cwd   string   `json:"cwd,omitempty"`
	RunAs string   `json:"run_as,omitempty"`
	// User is who `cyac shell` runs as, from the image's login-user label;
	// empty means root.
	User string `json:"user,omitempty"`
	// Expose is what `cyac expose` serves by default, from the image's
	// cyac.nullvoid.space/expose.<name>=<port> labels.
	Expose map[string]int `json:"expose,omitempty"`
}

// Store is where a kind of workload keeps its files, and its disks as
// ZFS zvols:
//
//	<dataset>/<name>               disk, at /dev/zvol/<dataset>/<name>
//	<dir>/disks/<name>.json        what seeding learned from the image (Box)
//	<dir>/run/<name>/              runtime files while running (bundle, sockets)
//	<dir>/logs/<name>.console.log  console, across runs
type Store struct {
	Dir     string
	Dataset string
}

func (s Store) DiskPath(name string) string { return "/dev/zvol/" + s.volume(name) }
func (s Store) RunDir(name string) string   { return filepath.Join(s.Dir, "run", name) }
func (s Store) ConsoleLog(name string) string {
	return filepath.Join(s.Dir, "logs", name+".console.log")
}
func (s Store) ConsoleIn(name string) string { return filepath.Join(s.RunDir(name), "console.in") }
