package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/containmentzone/cyacrt/internal/api"
	"github.com/containmentzone/cyacrt/internal/run"
)

// Exposing again keeps each port's token; regen replaces only the ports
// it names; ports dropped from the route lose theirs.
func TestTokensPersist(t *testing.T) {
	confDir := t.TempDir()
	for name, content := range map[string]string{
		"dev.cynetwork": "[Network]\nIndex=1\n",
		"a.cybox":       "[Box]\nSeedImage=i\nNetwork=dev\nAddress=10.42.1.10\n",
	} {
		if err := os.WriteFile(filepath.Join(confDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{
		Conf: confDir, Routes: filepath.Join(t.TempDir(), "routes"), Domain: "x.test",
		Run: &run.Runner{State: t.TempDir()}, RoutesChanged: func() error { return nil },
	}
	tokens := func(ports map[string]int) map[string]string {
		t.Helper()
		r, err := s.addRoute("a", api.Expose{Ports: ports})
		if err != nil {
			t.Fatal(err)
		}
		return r.Tokens
	}

	first := tokens(map[string]int{"web": 80, "api": 3000})
	if first["web"] == "" || first["api"] == "" || first["web"] == first["api"] {
		t.Fatalf("tokens %v", first)
	}
	again := tokens(map[string]int{"web": 8080, "api": 3000})
	if again["web"] != first["web"] || again["api"] != first["api"] {
		t.Errorf("re-expose changed tokens: %v → %v", first, again)
	}

	r, err := s.regenTokens("a", []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Tokens["web"] == first["web"] || r.Tokens["api"] != first["api"] {
		t.Errorf("regen web: %v → %v", first, r.Tokens)
	}
	if _, err := s.regenTokens("a", []string{"nope"}); err == nil {
		t.Error("regenerated a port the route doesn't have")
	}

	if got := tokens(map[string]int{"web": 80}); len(got) != 1 {
		t.Errorf("dropped port kept its token: %v", got)
	}
	if got := tokens(map[string]int{"web": 80, "api": 3000}); got["api"] == first["api"] {
		t.Error("a port that came back got its old token back")
	}
}
