package box

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
		b := &Defaults{Init: tc.init}
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

func TestTreeSize(t *testing.T) {
	dir := t.TempDir()
	if got, err := treeSize(dir); err != nil || got != 1<<30 {
		t.Errorf("empty tree: %d, %v; want 1 GiB", got, err)
	}
	// 100k entries need 100k inodes: 16 KiB each, plus a quarter.
	for i := range 100_000 {
		os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), nil, 0o644)
	}
	if got, _ := treeSize(dir); got < 100_000*16<<10 || got%(1<<20) != 0 {
		t.Errorf("100k entries: %d", got)
	}
}

// An image's zvol and file come and go together; get doesn't paper over
// one without the other.
func TestImagesInconsistent(t *testing.T) {
	id := v1.Hash{Algorithm: "sha256", Hex: "abc"}
	for _, tc := range []struct {
		name      string
		vol, file bool
	}{
		{name: "zvol without file", vol: true},
		{name: "file without zvol", file: true},
	} {
		log := fakeZFS(t, tc.vol)
		i := Images{Dir: t.TempDir(), Dataset: "tank/cyac/images"}
		if tc.file {
			os.WriteFile(i.file(id), []byte("{}"), 0o600)
		}
		if _, err := i.get(nil, id, func(string) {}); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
		if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "create") || strings.Contains(string(calls), "destroy") {
			t.Errorf("%s: touched zfs: %s", tc.name, calls)
		}
	}
}

func TestImagesUsed(t *testing.T) {
	id := v1.Hash{Algorithm: "sha256", Hex: "abc"}
	i := Images{Dir: t.TempDir()}
	rec := &Image{Defaults: Defaults{Init: []string{"/sbin/init"}}}
	if err := i.used(id, rec); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(i.Dir, "abc.json"))
	var got Image
	if err := json.Unmarshal(data, &got); err != nil || got.LastUsed.IsZero() || got.Init[0] != "/sbin/init" {
		t.Errorf("got %s, %v", data, err)
	}
}

// fakeZFS puts a zfs on PATH that logs its arguments and says every zvol
// is 1 GiB, or, without zvols, that there are none, and returns the log.
func fakeZFS(t *testing.T, zvols bool) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	get := "echo 1073741824"
	if !zvols {
		get = "exit 1"
	}
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n[ \"$1\" = get ] && " + get + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "zfs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return log
}
