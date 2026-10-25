// Package box manages boxes: per-box ext4 disks, booted by a runtime
// driver and attached by a network driver (see package driver).
package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/containmentzone/cyacrt/internal/driver"
)

// Box is the persisted record of one box. The disk itself lives next to it.
type Box struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	ID     int    `json:"id"` // small integer; derives the IP and interface names
	CPUs   int    `json:"cpus"`
	MemMiB int    `json:"mem_mib"`
	// Init is the image's ENTRYPOINT+CMD unless overridden, e.g.
	// /usr/local/bin/init for exeuntu, or /sbin/init if the image has
	// neither. Mode says what runs it (see agent.Config.Mode): "" makes it
	// PID 1, "command" runs it under cyac-init, and "pause" runs nothing.
	Init []string `json:"init"`
	Mode string   `json:"mode,omitempty"`
	// The image's environment, working directory and USER, for commands.
	Env   []string `json:"env,omitempty"`
	Cwd   string   `json:"cwd,omitempty"`
	RunAs string   `json:"run_as,omitempty"`
	// User is who `cyac shell` runs as, from the image's login-user label;
	// empty means root.
	User  string `json:"user,omitempty"`
	Route *Route `json:"route,omitempty"`
	// Digest is the image manifest the disk was seeded from, which keeps
	// its base disk from being pruned.
	Digest string `json:"digest,omitempty"`
	// Up says the box should be running: start sets it, and stop or the
	// box exiting on its own clears it. The daemon stops every box when it
	// exits and starts the ones that are up again when it starts.
	Up bool `json:"up,omitempty"`
}

// Route exposes one port of a box at <name>.<root domain>. Nothing is
// reachable without one.
type Route struct {
	Port      int    `json:"port"`
	Public    bool   `json:"public,omitempty"`
	TokenHash string `json:"token_hash,omitempty"` // hex sha256 of the bearer token
}

// Store is the on-disk state directory:
//
//	<dir>/boxes/<name>.json   box record
//	<dir>/disks/<name>.ext4   box disk
//	<dir>/bases/<digest>.ext4 an image's filesystem, which box disks start as a copy of
//	<dir>/run/<name>/         runtime files while running (bundle, sockets)
//	<dir>/logs/<name>.console.log  the box's console, across runs
type Store struct {
	Dir      string
	Net      driver.Network
	Runtimes []driver.Runtime // in order of preference; the first available one is used
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// ValidName reports whether name is usable as a box name (it ends up in
// paths, interface-adjacent names and hostnames).
func ValidName(name string) bool { return validName.MatchString(name) }

func (s Store) recordPath(name string) string { return filepath.Join(s.Dir, "boxes", name+".json") }
func (s Store) DiskPath(name string) string   { return filepath.Join(s.Dir, "disks", name+".ext4") }
func (s Store) BasePath(digest v1.Hash) string {
	return filepath.Join(s.Dir, "bases", digest.Hex+".ext4")
}
func (s Store) RunDir(name string) string { return filepath.Join(s.Dir, "run", name) }
func (s Store) ConsoleLog(name string) string {
	return filepath.Join(s.Dir, "logs", name+".console.log")
}
func (s Store) ConsoleIn(name string) string { return filepath.Join(s.RunDir(name), "console.in") }

// Lock takes an exclusive lock on the store for the life of the process,
// so only one daemon owns a state directory.
func (s Store) Lock() error {
	for _, d := range []string{"boxes", "disks", "bases", "run", "logs"} {
		if err := os.MkdirAll(filepath.Join(s.Dir, d), 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	// Released when the process exits.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s is in use by another cyac daemon", s.Dir)
	}
	return nil
}

func (s Store) Get(name string) (*Box, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("invalid box name %q", name)
	}
	data, err := os.ReadFile(s.recordPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no box named %q", name)
	} else if err != nil {
		return nil, err
	}
	var b Box
	return &b, json.Unmarshal(data, &b)
}

func (s Store) Put(b *Box) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.recordPath(b.Name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.recordPath(b.Name))
}

func (s Store) Delete(name string) error { return os.Remove(s.recordPath(name)) }

// List returns all boxes sorted by name.
func (s Store) List() ([]*Box, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "boxes", "*.json"))
	if err != nil {
		return nil, err
	}
	var boxes []*Box
	for _, f := range files {
		b, err := s.Get(filepath.Base(f[:len(f)-len(".json")]))
		if err != nil {
			return nil, err
		}
		boxes = append(boxes, b)
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Name < boxes[j].Name })
	return boxes, nil
}

// FreeID returns the lowest box ID not in use. IDs start at 2 so network
// drivers can keep low addresses (like a gateway) for themselves.
func (s Store) FreeID() (int, error) {
	boxes, err := s.List()
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, b := range boxes {
		used[b.ID] = true
	}
	for id := 2; id < 0xfffe; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, errors.New("out of box IDs")
}

// Prune removes the base disks of images no box was seeded from, and
// whatever a crashed seed left next to them, and returns the bytes that
// held. A box never reads its base again, so this only means the next box
// of a pruned image unpacks it again.
func (s Store) Prune() (int64, error) {
	boxes, err := s.List()
	if err != nil {
		return 0, err
	}
	keep := map[string]bool{}
	for _, b := range boxes {
		if h, err := v1.NewHash(b.Digest); err == nil {
			keep[s.BasePath(h)] = true
		}
	}
	paths, err := filepath.Glob(filepath.Join(s.Dir, "bases", "*"))
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, path := range paths {
		if keep[path] {
			continue
		}
		freed += allocated(path)
		if err := os.RemoveAll(path); err != nil {
			return freed, err
		}
	}
	return freed, nil
}

// allocated is the disk space the files under path take up.
func allocated(path string) int64 {
	var n int64
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			if fi, err := d.Info(); err == nil {
				n += fi.Sys().(*syscall.Stat_t).Blocks * 512
			}
		}
		return nil
	})
	return n
}
