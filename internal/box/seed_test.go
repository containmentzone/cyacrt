package box

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestPickMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sbinInit bool
		init     []string
		mode     string
		args     []string
		err      bool
	}{
		{name: "exeuntu", init: []string{"/usr/local/bin/init"}, args: []string{"/usr/local/bin/init"}},
		{name: "systemd", init: []string{"/lib/systemd/systemd"}, args: []string{"/lib/systemd/systemd"}},
		{name: "no command, has /sbin/init", sbinInit: true, args: []string{"/sbin/init"}},
		{name: "no command, no init", mode: "pause"},
		{name: "nginx", init: []string{"nginx", "-g", "daemon off;"}, mode: "command", args: []string{"nginx", "-g", "daemon off;"}},
		{name: "missing init", init: []string{"/usr/local/bin/init"}, err: true},
	} {
		dir := t.TempDir()
		for _, p := range []string{"bin/sh", "lib/systemd/systemd"} {
			os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
			os.WriteFile(filepath.Join(dir, p), nil, 0o755)
		}
		if tc.name == "exeuntu" {
			os.MkdirAll(filepath.Join(dir, "usr/local/bin"), 0o755)
			os.WriteFile(filepath.Join(dir, "usr/local/bin/init"), nil, 0o755)
		}
		if tc.sbinInit {
			os.MkdirAll(filepath.Join(dir, "sbin"), 0o755)
			os.Symlink("/lib/systemd/systemd", filepath.Join(dir, "sbin/init"))
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		b := &Box{Init: tc.init}
		err = pickMode(root, b)
		root.Close()
		if (err != nil) != tc.err {
			t.Errorf("%s: err %v", tc.name, err)
			continue
		}
		if !tc.err && (b.Mode != tc.mode || !slices.Equal(b.Init, tc.args)) {
			t.Errorf("%s: mode %q args %q, want %q %q", tc.name, b.Mode, b.Init, tc.mode, tc.args)
		}
	}
}

func TestPullTarball(t *testing.T) {
	img, err := mutate.Config(empty.Image, v1.Config{Entrypoint: []string{"/bin/x"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "img.tar")
	if err := tarball.WriteToFile(path, nil, img); err != nil {
		t.Fatal(err)
	}
	got, err := pull(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := got.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Config.Entrypoint, []string{"/bin/x"}) {
		t.Errorf("entrypoint %q", cfg.Config.Entrypoint)
	}
}

func TestExposes(t *testing.T) {
	got, err := exposes(map[string]string{
		"cyac.nullvoid.space/expose":      "8000", // no name: ignored
		"cyac.nullvoid.space/expose.pi":   "8787",
		"cyac.nullvoid.space/exposed":     "x", // not ours
		"cyac.nullvoid.space/login-user":  "ubuntu",
		"org.opencontainers.image.source": "x",
	})
	if err != nil || len(got) != 1 || got["pi"] != 8787 {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := exposes(map[string]string{"cyac.nullvoid.space/expose.web": "http"}); err == nil {
		t.Error("took a port that isn't a number")
	}
}
