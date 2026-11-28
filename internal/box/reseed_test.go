package box

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCarryFiles(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write := func(dir, name, content string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(src, "etc/machine-id", "old-id\n", 0o444)
	write(src, "etc/ssh/ssh_host_ed25519_key", "secret", 0o600)
	write(src, "etc/ssh/sshd_config", "from the old image", 0o644)
	write(src, "etc/passwd", "old users", 0o644)
	// A link out of the tree is not followed.
	if err := os.Symlink("/etc/shadow", filepath.Join(src, "etc/ssh/ssh_host_rsa_key")); err != nil {
		t.Fatal(err)
	}
	write(dst, "etc/machine-id", "", 0o444)
	write(dst, "etc/passwd", "new users", 0o644)

	if err := carryFiles(src, dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"etc/machine-id":               "old-id\n",
		"etc/ssh/ssh_host_ed25519_key": "secret",
		"etc/passwd":                   "new users",
	} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || string(got) != want {
			t.Errorf("%s: got %q (%v), want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"etc/ssh/sshd_config", "etc/ssh/ssh_host_rsa_key"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); err == nil {
			t.Errorf("%s was carried over", name)
		}
	}
	if fi, err := os.Stat(filepath.Join(dst, "etc/ssh/ssh_host_ed25519_key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode: %v, %v", fi.Mode(), err)
	}
}
