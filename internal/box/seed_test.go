package box

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
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
