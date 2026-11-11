package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containmentzone/cyacrt/internal/render"
)

// fake records the commands it is asked to run, and fails the ones that
// start with fail.
type fake struct {
	ran  []string
	fail string
}

func (f *fake) exec(s render.Step) error {
	cmd := strings.Join(s.Args, " ")
	f.ran = append(f.ran, cmd)
	if f.fail != "" && strings.HasPrefix(cmd, f.fail) {
		return errors.New("boom")
	}
	return nil
}

func runner(t *testing.T, conf map[string]string) (*Runner, *fake) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range conf {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{}
	return &Runner{Conf: dir, Dir: filepath.Join(t.TempDir(), "run"), Exec: f.exec}, f
}

const (
	out = "[Gateway]\nIndex=0\nKind=direct\n"
	dev = "[Network]\nIndex=1\nAttach=gateway:out\n"
)

// Stop runs what Start recorded, even after the file is gone.
func TestStartStop(t *testing.T) {
	r, f := runner(t, map[string]string{"dev.cynetwork": dev, "out.cygateway": out})
	if err := r.Start(context.Background(), "cynetwork", "dev"); err != nil {
		t.Fatal(err)
	}
	if len(f.ran) == 0 || f.ran[0] != "ip link add cyn1 type bridge" {
		t.Fatalf("Up ran %q", f.ran)
	}
	if err := os.Remove(filepath.Join(r.Conf, "dev.cynetwork")); err != nil {
		t.Fatal(err)
	}
	f.ran = nil
	if err := r.Stop("cynetwork", "dev"); err != nil {
		t.Fatal(err)
	}
	want := []string{"nft delete table inet cyn-dev", "ip link del cynl1", "ip link del cyn1"}
	if strings.Join(f.ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("Down ran %q, want %q", f.ran, want)
	}
	f.ran = nil
	if err := r.Stop("cynetwork", "dev"); err != nil || len(f.ran) != 0 {
		t.Errorf("second Stop ran %q, %v", f.ran, err)
	}
}

// A failed Up is undone, and the record goes with it.
func TestStartFails(t *testing.T) {
	r, f := runner(t, map[string]string{"out.cygateway": out})
	f.fail = "ip -n cygw-out route add"
	err := r.Start(context.Background(), "cygateway", "out")
	if err == nil || !strings.Contains(err.Error(), "route add default") {
		t.Fatalf("err = %v", err)
	}
	if last := f.ran[len(f.ran)-1]; last != "ip netns del cygw-out" {
		t.Errorf("last step %q, want the netns removed", last)
	}
	if _, err := os.Stat(r.path("cygateway", "out")); !os.IsNotExist(err) {
		t.Errorf("record left behind: %v", err)
	}
}

// A start after a crash first clears what the crashed run left.
func TestStartClearsOld(t *testing.T) {
	r, f := runner(t, map[string]string{"out.cygateway": out})
	if err := r.Start(context.Background(), "cygateway", "out"); err != nil {
		t.Fatal(err)
	}
	f.ran = nil
	if err := r.Start(context.Background(), "cygateway", "out"); err != nil {
		t.Fatal(err)
	}
	if f.ran[0] != "nft delete table inet cygw-out" || !strings.HasPrefix(f.ran[3], "ip netns add") {
		t.Errorf("ran %q", f.ran)
	}
}

func TestStartRefuses(t *testing.T) {
	r, f := runner(t, map[string]string{
		"out.cygateway": out,
		"bad.cynetwork": "[Network]\nIndex=2\nAttach=gateway:gone\n",
	})
	for _, tc := range []struct{ kind, name, want string }{
		{"cynetwork", "bad", "gone"},
		{"cynetwork", "nope", "no cynetwork nope"},
		{"cybox", "a", "no cybox a"},
		{"cyfoo", "a", "unknown kind"},
	} {
		if err := r.Start(context.Background(), tc.kind, tc.name); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Start(%s, %s) = %v, want %q", tc.kind, tc.name, err, tc.want)
		}
	}
	if err := r.Stop("cygateway", "../x"); err == nil {
		t.Error("Stop took a path as a name")
	}
	if len(f.ran) != 0 {
		t.Errorf("ran %q", f.ran)
	}
}
