package proxy

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Routes come from the route dir, addresses from the conf, and a route
// whose box has no file isn't served. Each named port gets its own host.
func TestReload(t *testing.T) {
	confDir, routes := t.TempDir(), filepath.Join(t.TempDir(), "routes")
	for name, content := range map[string]string{
		"dev.cynetwork": "[Network]\nIndex=1\n",
		"a.cybox":       "[Box]\nSeedImage=i\nNetwork=dev\nAddress=10.42.1.10\n",
		"b.cybox":       "[Box]\nSeedImage=i\nNetwork=dev\nAddress=10.42.1.11\n",
	} {
		if err := os.WriteFile(filepath.Join(confDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveRoute(routes, "a", &Route{Port: 8080, Public: true}); err != nil { // before named ports
		t.Fatal(err)
	}
	if err := SaveRoute(routes, "b", &Route{Ports: map[string]int{"": 80, "api": 3000}, TokenHash: "h"}); err != nil {
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
	for host, port := range map[string]int{
		"b.boxes.example.com":     80,
		"api.b.boxes.example.com": 3000,
	} {
		tg, ok := s.lookup(host)
		if !ok || tg.Port != port || tg.TokenHash != "h" || tg.IP.String() != "10.42.1.11" {
			t.Errorf("%s: %+v, %v", host, tg, ok)
		}
		if err := s.HostPolicy(context.Background(), host); err != nil {
			t.Errorf("%s: no certificate: %v", host, err)
		}
	}
	if _, ok := s.lookup("x.b.boxes.example.com"); ok {
		t.Error("served an unnamed port")
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

// The bare root domain gets the splash page, and a certificate; anything
// else on it, and unknown hosts, stay 404.
func TestSplash(t *testing.T) {
	s := &Server{Domain: "Boxes.Example.com"}
	for _, c := range []struct {
		host, path string
		code       int
	}{
		{"boxes.example.com", "/", 200},
		{"BOXES.example.com:443", "/", 200},
		{"boxes.example.com", "/x", 404},
		{"nope.boxes.example.com", "/", 404},
		{"example.com", "/", 404},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "http://"+c.host+c.path, nil))
		if w.Code != c.code {
			t.Errorf("%s%s: %d, want %d", c.host, c.path, w.Code, c.code)
		}
		if c.code == 200 && !strings.Contains(w.Body.String(), "Everything runs on CyberAcme") {
			t.Errorf("%s: no splash", c.host)
		}
	}
	if err := s.HostPolicy(context.Background(), "boxes.example.com"); err != nil {
		t.Errorf("root domain refused: %v", err)
	}
	if err := s.HostPolicy(context.Background(), "nope.boxes.example.com"); err == nil {
		t.Error("unknown host allowed")
	}
}
