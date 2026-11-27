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
	if err := fillVolume(root, data, "/home", quiet); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(data, "home/ubuntu/.profile")); err != nil || string(got) != "hi" {
		t.Errorf("home not filled: %q, %v", got, err)
	}
	if err := fillVolume(root, data, "/var/lib/tailscale", quiet); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(data, "var-lib-tailscale")); err != nil || !fi.IsDir() {
		t.Errorf("missing path not made empty: %v", err)
	}
	if err := fillVolume(root, data, "/escape/ssl", quiet); err == nil {
		t.Error("followed a symlink out of the root")
	}
	// A directory the data disk has already is left alone.
	os.WriteFile(filepath.Join(root, "home/ubuntu/.profile"), []byte("changed"), 0o644)
	if err := fillVolume(root, data, "/home", quiet); err != nil {
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
	if err := fillVolume(root, data, "/home/x", quiet); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "home-x.tmp/keep")); err != nil {
		t.Errorf("another volume was touched: %v", err)
	}
	// A symlink the box left on the data disk is no volume.
	if err := os.Symlink("/etc", filepath.Join(data, "srv")); err != nil {
		t.Fatal(err)
	}
	if err := fillVolume(root, data, "/srv", quiet); err == nil {
		t.Error("took a symlink on the data disk")
	}
}
