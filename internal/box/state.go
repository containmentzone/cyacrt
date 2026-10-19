// Package box manages boxes: per-box ext4 disks booted either as a
// Firecracker VM or as a systemd container under runc.
package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
)

// Box is the persisted record of one box. The disk itself lives next to it.
type Box struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	ID     int    `json:"id"` // small integer; derives the IP and interface names
	CPUs   int    `json:"cpus"`
	MemMiB int    `json:"mem_mib"`
	// Init is PID 1: the image's ENTRYPOINT+CMD unless overridden, e.g.
	// /usr/local/bin/init for exeuntu or /lib/systemd/systemd.
	Init   []string `json:"init"`
	Routes []Route  `json:"routes,omitempty"`
}

// Route is one explicitly exposed hostname. Nothing is reachable without one.
type Route struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Public    bool   `json:"public,omitempty"`
	TokenHash string `json:"token_hash,omitempty"` // hex sha256 of the bearer token
}

// Store is the on-disk state directory:
//
//	<dir>/boxes/<name>.json   box record
//	<dir>/disks/<name>.ext4   box disk
//	<dir>/run/<name>/         runtime files while running (bundle, sockets, logs)
type Store struct{ Dir string }

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// ValidName reports whether name is usable as a box name (it ends up in
// paths, interface-adjacent names and hostnames).
func ValidName(name string) bool { return validName.MatchString(name) }

func (s Store) recordPath(name string) string { return filepath.Join(s.Dir, "boxes", name+".json") }
func (s Store) DiskPath(name string) string   { return filepath.Join(s.Dir, "disks", name+".ext4") }
func (s Store) RunDir(name string) string     { return filepath.Join(s.Dir, "run", name) }

// Lock takes an exclusive lock on the store for the life of the process.
// Every mutating command calls it first.
func (s Store) Lock() error {
	for _, d := range []string{"boxes", "disks", "run"} {
		if err := os.MkdirAll(filepath.Join(s.Dir, d), 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) // released when the process exits
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

// FreeID returns the lowest box ID not in use. IDs start at 2 because
// 10.88.0.1 is the host side of the bridge.
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

// Network layout: one bridge, 10.88.0.0/16, host at 10.88.0.1.
const (
	BridgeName = "cyac0"
	PrefixLen  = 16
)

var HostIP = netip.MustParseAddr("10.88.0.1")

func (b *Box) IP() netip.Addr {
	return netip.AddrFrom4([4]byte{10, 88, byte(b.ID >> 8), byte(b.ID)})
}

// ifName is the host-side interface name (tap or veth); Linux caps names at 15 bytes.
func (b *Box) ifName() string { return fmt.Sprintf("ax%d", b.ID) }
