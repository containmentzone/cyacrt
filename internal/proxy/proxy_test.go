package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// Routes come from the route dir, addresses from the conf, and a route
// whose box has no file isn't served.
func TestReload(t *testing.T) {
	confDir, routes := t.TempDir(), filepath.Join(t.TempDir(), "routes")
	for name, content := range map[string]string{
		"dev.cynetwork": "[Network]\nIndex=1\n",
		"a.cybox":       "[Workload]\nImage=i\n\n[Box]\nNetwork=dev\nAddress=10.42.1.10\n",
	} {
		if err := os.WriteFile(filepath.Join(confDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveRoute(routes, "a", &Route{Port: 8080, Public: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveRoute(routes, "gone", &Route{Port: 80}); err != nil {
		t.Fatal(err)
	}
	if err := SaveRoute(routes, "../x", &Route{Port: 80}); err == nil {
		t.Error("saved a route under a path")
	}

	s := &Server{Conf: confDir, Routes: routes, Domain: "Boxes.Example.com"}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if tg, ok := s.lookup("A.boxes.example.com:443"); !ok || tg.Port != 8080 || tg.IP.String() != "10.42.1.10" {
		t.Errorf("a: %+v, %v", tg, ok)
	}
	if _, ok := s.lookup("gone.boxes.example.com"); ok {
		t.Error("served a route without a box")
	}

	if err := SaveRoute(routes, "a", nil); err != nil {
		t.Fatal(err)
	}
	s.Reload()
	if _, ok := s.lookup("a.boxes.example.com"); ok {
		t.Error("removed route still served")
	}
}
