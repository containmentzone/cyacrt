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
	// ImageID is the ID of the image Image resolved to when the disk was
	// seeded: the digest of its config, which is per platform.
	ImageID string `json:"image_id,omitempty"`
	// MachineID is the box's /etc/machine-id, written into it on every
	// start (see agent.WriteNetConfig). It is made when the disk is first
	// seeded and kept through reseeds.
	MachineID string `json:"machine_id,omitempty"`
	Defaults
	// Kept are the volumes the box's data disk holds, each filled once,
	// when it first appeared (see FillVolumes). A volume stays mounted
	// even if the image and the file stop naming it.
	Kept []string `json:"kept_volumes,omitempty"`
	// FillFrom names the disk a reseed just replaced (<name>.prev). A
	// volume new since then is filled from it first, so data a box kept
	// on its root moves to the data disk instead of being lost; the next
	// FillVolumes clears it.
	FillFrom string `json:"fill_from,omitempty"`
}

// Defaults are what an image says to run and how, the same for every
// workload seeded from it.
type Defaults struct {
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
	// KeepRoot is the image's cyac.nullvoid.space/root=keep label: a disk
	// seeded from it is never reseeded when the workload's image changes,
	// unless the workload says Root=replace.
	KeepRoot bool `json:"keep_root,omitempty"`
	// Volumes are the image's VOLUMEs, sorted.
	Volumes []string `json:"volumes,omitempty"`
}

// Store is where a kind of workload keeps its files, and its disks as
// ZFS zvols:
//
//	<dataset>/<name>               disk, at /dev/zvol/<dataset>/<name>,
//	                               usually a clone of an image (see Images)
//	<dataset>/<name>.new           the disk a reseed is making (see Reseed)
//	<dataset>/<name>.prev          the disk before the last reseed
//	<dataset>/<name>.data          a box's data disk: its volumes, which
//	                               outlive reseeds (see FillVolumes)
//	<dir>/disks/<name>.json        what seeding learned from the image (Box)
//	<dir>/run/<name>/              runtime files while running (bundle, sockets)
//	<dir>/logs/<name>.console.log  console, across runs
type Store struct {
	Dir     string
	Dataset string
	Images  Images
}

func (s Store) DiskPath(name string) string { return device(s.volume(name)) }
func (s Store) DataPath(name string) string { return device(s.volume(name + ".data")) }
func (s Store) RunDir(name string) string   { return filepath.Join(s.Dir, "run", name) }
func (s Store) ConsoleLog(name string) string {
	return filepath.Join(s.Dir, "logs", name+".console.log")
}
func (s Store) ConsoleIn(name string) string { return filepath.Join(s.RunDir(name), "console.in") }
