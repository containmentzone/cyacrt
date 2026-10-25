package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestPrune(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Lock(); err != nil {
		t.Fatal(err)
	}
	used := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("a", 64)}
	unused := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("b", 64)}
	for _, h := range []v1.Hash{used, unused} {
		os.WriteFile(s.BasePath(h), make([]byte, 8192), 0o600)
	}
	leftover := filepath.Join(s.Dir, "bases", "cc.seed-1")
	os.MkdirAll(filepath.Join(leftover, "etc"), 0o755)
	if err := s.Put(&Box{Name: "web", Digest: used.String()}); err != nil {
		t.Fatal(err)
	}
	freed, err := s.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if freed < 8192 {
		t.Errorf("freed %d bytes, want at least 8192", freed)
	}
	for path, want := range map[string]bool{s.BasePath(used): true, s.BasePath(unused): false, leftover: false} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", path, err == nil, want)
		}
	}
}
