package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteNetConfig(t *testing.T) {
	dir := t.TempDir()
	// An image's resolv.conf is often a symlink out of the root; it must be
	// replaced, not followed.
	outside := filepath.Join(t.TempDir(), "stub")
	os.WriteFile(outside, []byte("stub"), 0o644)
	os.MkdirAll(filepath.Join(dir, "etc/systemd/network"), 0o755)
	os.Symlink(outside, filepath.Join(dir, "etc/resolv.conf"))
	os.WriteFile(filepath.Join(dir, "etc/systemd/network/10-cyac.network"), []byte("Address=10.88.0.2/16"), 0o644)
	os.WriteFile(filepath.Join(dir, "etc/machine-id"), []byte("picked-by-the-box\n"), 0o444)

	if err := WriteNetConfig(dir, Config{DNS: "1.1.1.1", Network: "Address=10.42.0.2/16\n", MachineID: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"etc/resolv.conf":                     "# written by cyac on every start\nnameserver 1.1.1.1\n",
		"etc/systemd/network/10-cyac.network": "Address=10.42.0.2/16\n",
		"etc/machine-id":                      "0123456789abcdef0123456789abcdef\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if got, _ := os.ReadFile(outside); string(got) != "stub" {
		t.Errorf("followed the symlink: %q", got)
	}
}

func TestWriteNetConfigHostname(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "etc"), 0o755)
	// A box that set its own hosts keeps them; a missing hostname is written.
	os.WriteFile(filepath.Join(dir, "etc/hosts"), []byte("mine\n"), 0o644)
	if err := WriteNetConfig(dir, Config{Hostname: "a"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"etc/hostname": "a\n", "etc/hosts": "mine\n"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestVolumeDir(t *testing.T) {
	for in, want := range map[string]string{
		"/home":               "home",
		"/var/lib/docker":     "var-lib-docker",
		"/var/lib/my-app":     `var-lib-my\x2dapp`,
		"/srv/.hidden":        "srv-.hidden",
		"/.dot":               `\x2edot`,
		"/var/lib/tailscale/": "var-lib-tailscale",
	} {
		if got := VolumeDir(in); got != want {
			t.Errorf("VolumeDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVolumeOK(t *testing.T) {
	for p, want := range map[string]bool{
		"/home": true, "/var/lib/docker": true, "/etc/nginx": true, "/etc/ssh": true,
		"/": false, "home": false, "/home/": false, "/a/../b": false, "../etc": false,
		"/run": true, "/etc": true,
	} {
		if got := VolumeOK(p); got != want {
			t.Errorf("VolumeOK(%q) = %v, want %v", p, got, want)
		}
	}
}
