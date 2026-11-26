// Package lease hands networks, gateways and services the numbers that
// pick their addresses and interface names, so nobody writes them into
// the files. A lease belongs to an object by kind and name and is sticky:
// the object keeps it across restarts and reboots, and while the object is
// still running nobody else can take it, even after its file changed or
// went away. Boxes and services find each other by name in DNS, so which
// number an object gets doesn't matter, only that it stays put.
//
// Each lease is a file, <dir>/<pool>/<number>, holding its owner. A freed
// lease keeps its file, emptied, so its mtime says when it was freed: a
// new owner gets a number that was never used if there is one, and
// otherwise the one freed longest ago, so a number a stale peer may still
// remember is reused last.
package lease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containmentzone/cyacrt/internal/conf"
)

// Live says whether kind/name is running, and so must keep its leases
// even if its file no longer asks for them.
type Live func(kind, name string) bool

// Assign gives every object in c its numbers: Index for networks,
// gateways and services, each service's Octets on the networks that
// attach it, and each box's Address. It frees leases whose owner neither
// is in c nor is running, then hands out what is missing; a full pool is
// an error. It holds a lock on dir throughout, as units start in
// parallel.
func Assign(dir string, c *conf.Config, live Live) error { return assign(dir, c, live) }

// Read fills in the numbers objects in c already hold, and changes no
// lease. An object without one isn't running and keeps zero numbers (a
// box: an invalid Address). It is for readers such as the proxy.
func Read(dir string, c *conf.Config) error { return assign(dir, c, nil) }

// assign is Assign, or with no live, Read.
func assign(dir string, c *conf.Config, live Live) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	how := syscall.LOCK_EX
	if live == nil {
		how = syscall.LOCK_SH
	}
	if err := syscall.Flock(int(lock.Fd()), how); err != nil {
		return err
	}

	type want struct {
		pool, owner string
		min, max    int
		set         func(int)
	}
	var wants []want
	for _, name := range conf.Sorted(c.Networks) {
		n := c.Networks[name]
		wants = append(wants, want{"network", conf.KindNetwork + "/" + name, 0, conf.MaxNetworkIndex, func(i int) { n.Index = i }})
	}
	for _, name := range conf.Sorted(c.Gateways) {
		g := c.Gateways[name]
		wants = append(wants, want{"gateway", conf.KindGateway + "/" + name, 0, conf.MaxLinkIndex, func(i int) { g.Index = i }})
	}
	for _, name := range conf.Sorted(c.Services) {
		s := c.Services[name]
		s.Octets = map[string]byte{}
		wants = append(wants, want{"service", conf.KindService + "/" + name, 0, conf.MaxLinkIndex, func(i int) { s.Index = i }})
		// A service's address on a network is a lease in that network's
		// own pool, .3 to .9.
		for _, net := range c.AttachedBy(name) {
			wants = append(wants, want{"service-on-" + net, conf.KindService + "/" + name, 3, 2 + conf.MaxServices,
				func(i int) { s.Octets[net] = byte(i) }})
		}
	}
	// Networks come first in wants, so a box's network has its Index by
	// the time the box gets its address.
	for _, name := range conf.Sorted(c.Boxes) {
		b := c.Boxes[name]
		wants = append(wants, want{"box-on-" + b.Network, conf.KindBox + "/" + name, 10, 254,
			func(i int) { b.Address = c.Networks[b.Network].Addr(byte(i)) }})
	}

	pools := map[string]map[string]bool{} // pool -> owners in c
	for _, w := range wants {
		if pools[w.pool] == nil {
			pools[w.pool] = map[string]bool{}
		}
		pools[w.pool][w.owner] = true
	}
	// Free what nothing in c wants and nothing running holds. A pool no
	// one in c uses any more is a network that went away; its leases go
	// too, once their services stopped.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && live != nil {
			if err := free(filepath.Join(dir, e.Name()), pools[e.Name()], live); err != nil {
				return err
			}
		}
	}

	for _, w := range wants {
		i, err := take(filepath.Join(dir, w.pool), w.owner, w.min, w.max, live != nil)
		if err != nil {
			return err
		}
		if i >= 0 {
			w.set(i)
		}
	}
	return nil
}

// free empties every lease in pool whose owner isn't in want and isn't
// running.
func free(pool string, want map[string]bool, live Live) error {
	held, err := read(pool)
	if err != nil {
		return err
	}
	for i, owner := range held {
		if owner == "" {
			continue
		}
		if want[owner] {
			continue
		}
		kind, name, _ := strings.Cut(owner, "/")
		if live(kind, name) {
			continue
		}
		if err := os.WriteFile(filepath.Join(pool, strconv.Itoa(i)), nil, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// take returns owner's lease in pool, making one if it has none and
// make is set, or else returning -1.
func take(pool, owner string, min, max int, make bool) (int, error) {
	held, err := read(pool)
	if err != nil {
		return 0, err
	}
	for i, o := range held {
		if o == owner {
			return i, nil
		}
	}
	if !make {
		return -1, nil
	}
	if err := os.MkdirAll(pool, 0o700); err != nil {
		return 0, err
	}
	pick, oldest := -1, time.Time{}
	for i := min; i <= max; i++ {
		o, used := held[i]
		if !used {
			pick = i // never used: best
			break
		}
		if o != "" {
			continue
		}
		fi, err := os.Stat(filepath.Join(pool, strconv.Itoa(i)))
		if err != nil {
			return 0, err
		}
		if pick < 0 || fi.ModTime().Before(oldest) {
			pick, oldest = i, fi.ModTime()
		}
	}
	if pick < 0 {
		return 0, fmt.Errorf("%s: no free lease for %s (%d to %d all taken)", filepath.Base(pool), owner, min, max)
	}
	return pick, os.WriteFile(filepath.Join(pool, strconv.Itoa(pick)), []byte(owner), 0o600)
}

// read returns pool's leases, number to owner; a freed one has owner "".
func read(pool string) (map[int]string, error) {
	entries, err := os.ReadDir(pool)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	held := map[int]string{}
	for _, e := range entries {
		i, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pool, e.Name()))
		if err != nil {
			return nil, err
		}
		held[i] = string(data)
	}
	return held, nil
}
