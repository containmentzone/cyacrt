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

	if err := WriteNetConfig(dir, Config{DNS: "1.1.1.1", Network: "Address=10.42.0.2/16\n"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"etc/resolv.conf":                     "# written by cyac on every start\nnameserver 1.1.1.1\n",
		"etc/systemd/network/10-cyac.network": "Address=10.42.0.2/16\n",
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
