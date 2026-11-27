package box

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestVolumes(t *testing.T) {
	b := &Box{Kept: []string{"/old"}, Defaults: Defaults{Volumes: []string{"/var/lib/docker", "/home", "../../etc", "/run/x", "/etc"}}}
	got := Volumes(b, []string{"/home/", "/srv"})
	want := []string{"/old", "/srv", "/home", "/var/lib/docker"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFillVolume(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "home/ubuntu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "home/ubuntu/.profile"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	quiet := func(string) {}
	if err := fillVolume(sources(nil, data, []string{root}, "/home"), data, "/home", quiet); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(data, "home/ubuntu/.profile")); err != nil || string(got) != "hi" {
		t.Errorf("home not filled: %q, %v", got, err)
	}
	if err := fillVolume(sources(nil, data, []string{root}, "/var/lib/tailscale"), data, "/var/lib/tailscale", quiet); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(data, "var-lib-tailscale")); err != nil || !fi.IsDir() {
		t.Errorf("missing path not made empty: %v", err)
	}
	if err := fillVolume(sources(nil, data, []string{root}, "/escape/ssl"), data, "/escape/ssl", quiet); err == nil {
		t.Error("followed a symlink out of the root")
	}
	// A directory the data disk has already is left alone.
	os.WriteFile(filepath.Join(root, "home/ubuntu/.profile"), []byte("changed"), 0o644)
	if err := fillVolume(sources(nil, data, []string{root}, "/home"), data, "/home", quiet); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(data, "home/ubuntu/.profile")); string(got) != "hi" {
		t.Errorf("refilled: %q", got)
	}
	// Filling /home/x doesn't touch the volume /home/x.tmp.
	if err := os.MkdirAll(filepath.Join(root, "home/x"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(data, "home-x.tmp"), 0o755)
	os.WriteFile(filepath.Join(data, "home-x.tmp/keep"), nil, 0o644)
	if err := fillVolume(sources(nil, data, []string{root}, "/home/x"), data, "/home/x", quiet); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "home-x.tmp/keep")); err != nil {
		t.Errorf("another volume was touched: %v", err)
	}
	// A symlink the box left on the data disk is no volume.
	if err := os.Symlink("/etc", filepath.Join(data, "srv")); err != nil {
		t.Fatal(err)
	}
	if err := fillVolume(sources(nil, data, []string{root}, "/srv"), data, "/srv", quiet); err == nil {
		t.Error("took a symlink on the data disk")
	}
}

// After a reseed, a new volume comes from the old root where it has the
// path, and from the new root where it doesn't.
func TestFillVolumeAfterReseed(t *testing.T) {
	old, root, data := t.TempDir(), t.TempDir(), t.TempDir()
	for f, s := range map[string]string{
		filepath.Join(old, "home/u/notes"):      "mine",
		filepath.Join(root, "home/u/.profile"):  "image",
		filepath.Join(root, "var/log/skeleton"): "image",
	} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	quiet := func(string) {}
	for _, v := range []string{"/home", "/var/log"} {
		if err := fillVolume(sources(nil, data, []string{old, root}, v), data, v, quiet); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(data, "home/u/notes")); string(got) != "mine" {
		t.Errorf("home not taken from the old root: %q", got)
	}
	if _, err := os.Stat(filepath.Join(data, "home/u/.profile")); err == nil {
		t.Error("home mixed in the new root")
	}
	if got, _ := os.ReadFile(filepath.Join(data, "var-log/skeleton")); string(got) != "image" {
		t.Errorf("var/log not taken from the new root: %q", got)
	}
}

func TestMountable(t *testing.T) {
	got := Mountable([]string{"/home", "/run/app", "/etc", "/var/log"})
	if want := []string{"/home", "/var/log"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseExt4Size(t *testing.T) {
	out := "Filesystem volume name:   cyac-data\nBlock count:              2621440\nBlock size:               4096\n"
	if got, err := parseExt4Size(out); err != nil || got != 10<<30 {
		t.Errorf("got %d, %v", got, err)
	}
	if _, err := parseExt4Size("nothing"); err == nil {
		t.Error("no error for output without sizes")
	}
}

// A volume inside a kept one starts with what the kept one has there.
func TestFillNestedVolume(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "home/u"), 0o755)
	os.WriteFile(filepath.Join(root, "home/u/f"), []byte("image"), 0o644)
	os.MkdirAll(filepath.Join(data, "home/u"), 0o755)
	os.WriteFile(filepath.Join(data, "home/u/f"), []byte("mine"), 0o644)
	srcs := sources([]string{"/home"}, data, []string{root}, "/home/u")
	if err := fillVolume(srcs, data, "/home/u", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(data, "home-u/f")); string(got) != "mine" {
		t.Errorf("got %q, want the kept /home's copy", got)
	}
}
