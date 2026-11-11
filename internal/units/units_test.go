package units

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containmentzone/cyacrt/internal/conf"
)

var update = flag.Bool("update", false, "rewrite testdata/units.golden")

// TestGenerate renders testdata/conf and compares every file and symlink
// with testdata/units.golden, which is meant to be read in review.
func TestGenerate(t *testing.T) {
	c, probs, err := conf.Load("testdata/conf")
	if err != nil {
		t.Fatal(err)
	}
	o := Generate(c, probs, Paths{Cyac: "/bin/cyac", Generator: "/bin/cyac-generator", ConfDir: conf.Dir})

	var b strings.Builder
	for _, name := range conf.Sorted(o.Files) {
		b.WriteString("==> " + name + " <==\n" + o.Files[name] + "\n")
	}
	for _, name := range conf.Sorted(o.Symlinks) {
		b.WriteString("==> " + name + " -> " + o.Symlinks[name] + "\n")
	}
	golden := filepath.Join("testdata", "units.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("output differs from %s; run go test ./internal/units -update and review the diff", golden)
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	o := Output{
		Files:    map[string]string{"cyac.target": "x", "cybox@a.service": "y"},
		Symlinks: map[string]string{"cyac.target.wants/cybox@a.service": "../cybox@a.service"},
	}
	if err := Write(dir, o); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "cyac.target.wants", "cybox@a.service"))
	if err != nil || string(got) != "y" {
		t.Fatalf("symlink resolves to %q, %v", got, err)
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"cyac.target", "cyac.target.wants", "cybox@a.service"}) {
		t.Fatalf("dir holds %v", names)
	}
}
