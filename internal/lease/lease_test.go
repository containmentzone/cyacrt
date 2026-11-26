package lease

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containmentzone/cyacrt/internal/conf"
)

func load(t *testing.T, files map[string]string) *conf.Config {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, probs, err := conf.Load(dir)
	if err != nil || len(probs) != 0 {
		t.Fatalf("%v %v", probs, err)
	}
	return c
}

var base = map[string]string{
	"out.cygateway":   "[Gateway]\nKind=direct\n",
	"dev.cynetwork":   "[Network]\nAttach=gateway:out\nAttach=service:proxy\n",
	"proxy.cyservice": "[Service]\nSeedImage=i\nGateway=out\nPort=80\n",
	"a.cybox":         "[Box]\nSeedImage=i\nNetwork=dev\n",
}

func with(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range base {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func none(kind, name string) bool { return false }

func TestAssign(t *testing.T) {
	dir := t.TempDir()
	c := load(t, base)
	if err := Assign(dir, c, none); err != nil {
		t.Fatal(err)
	}
	n, s := c.Networks["dev"], c.Services["proxy"]
	if got := c.Boxes["a"].Address.String(); got != "10.42.0.10" {
		t.Errorf("box a at %s", got)
	}
	if got := s.AddressIn(n).String(); got != "10.42.0.3" {
		t.Errorf("proxy at %s", got)
	}

	// Sticky: a new network and box that sort first get new numbers;
	// nothing that had one moves.
	c = load(t, with(map[string]string{
		"aa.cynetwork": "[Network]\n",
		"aa.cybox":     "[Box]\nSeedImage=i\nNetwork=dev\n",
	}))
	if err := Assign(dir, c, none); err != nil {
		t.Fatal(err)
	}
	if c.Networks["dev"].Index != 0 || c.Networks["aa"].Index != 1 {
		t.Errorf("dev %d, aa %d", c.Networks["dev"].Index, c.Networks["aa"].Index)
	}
	if c.Boxes["a"].Address.String() != "10.42.0.10" || c.Boxes["aa"].Address.String() != "10.42.0.11" {
		t.Errorf("a %s, aa %s", c.Boxes["a"].Address, c.Boxes["aa"].Address)
	}

	// Read sees the same, and gives nothing to an object without a lease.
	r := load(t, with(map[string]string{"new.cybox": "[Box]\nSeedImage=i\nNetwork=dev\n"}))
	if err := Read(dir, r); err != nil {
		t.Fatal(err)
	}
	if r.Boxes["a"].Address.String() != "10.42.0.10" || r.Boxes["new"].Address.IsValid() {
		t.Errorf("read: a %s, new %s", r.Boxes["a"].Address, r.Boxes["new"].Address)
	}
}

// A running object keeps its lease after its file is gone; once it
// stopped, its number is free again, but a never-used one goes first.
func TestFree(t *testing.T) {
	dir := t.TempDir()
	two := with(map[string]string{"b.cybox": "[Box]\nSeedImage=i\nNetwork=dev\n"})
	if err := Assign(dir, load(t, two), none); err != nil {
		t.Fatal(err)
	}
	// a is .10, b is .11. a's file goes while a still runs.
	gone := with(map[string]string{"b.cybox": two["b.cybox"], "c.cybox": two["b.cybox"]})
	delete(gone, "a.cybox")
	c := load(t, gone)
	running := func(kind, name string) bool { return kind == conf.KindBox && name == "a" }
	if err := Assign(dir, c, running); err != nil {
		t.Fatal(err)
	}
	if got := c.Boxes["c"].Address.String(); got != "10.42.0.12" {
		t.Errorf("c took %s while a still runs on .10", got)
	}
	// a stopped: .10 is freed, but d still gets the never-used .13.
	c = load(t, with(map[string]string{"b.cybox": two["b.cybox"], "c.cybox": two["b.cybox"], "d.cybox": two["b.cybox"]}))
	delete(c.Boxes, "a")
	if err := Assign(dir, c, none); err != nil {
		t.Fatal(err)
	}
	if got := c.Boxes["d"].Address.String(); got != "10.42.0.13" {
		t.Errorf("d got %s, want the never-used .13", got)
	}
}

// Once every number was used, the one freed longest ago goes first.
func TestOldestFreed(t *testing.T) {
	pool := filepath.Join(t.TempDir(), "p")
	for _, owner := range []string{"x/1", "x/2", "x/3"} {
		if _, err := take(pool, owner, 0, 2, true); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	for i, when := range map[string]time.Time{"1": time.Now(), "2": old} {
		path := filepath.Join(pool, i)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	if i, err := take(pool, "x/4", 0, 2, true); err != nil || i != 2 {
		t.Errorf("got %d, %v; want 2, freed longest ago", i, err)
	}
	if i, err := take(pool, "x/5", 0, 2, true); err != nil || i != 1 {
		t.Errorf("got %d, %v; want 1", i, err)
	}
	if _, err := take(pool, "x/6", 0, 2, true); err == nil {
		t.Error("took a lease from a full pool")
	}
}
