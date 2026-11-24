package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Routes come from the route dir, addresses from the conf, and a route
// whose box has no file isn't served. Each named port gets its own host,
// and the bare box host gets nothing.
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
	if err := SaveRoute(routes, "a", &Route{Ports: map[string]int{"web": 8080}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveRoute(routes, "b", &Route{Ports: map[string]int{"ui": 80, "api": 3000}, TokenHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveRoute(routes, "gone", &Route{Ports: map[string]int{"web": 80}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveRoute(routes, "../x", &Route{Ports: map[string]int{"web": 80}}); err == nil {
		t.Error("saved a route under a path")
	}

	s := &Server{Conf: confDir, Routes: routes, Domain: "Boxes.Example.com"}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if tg, ok := s.lookup("Web.A.boxes.example.com:443"); !ok || tg.Port != 8080 || tg.IP.String() != "10.42.1.10" {
		t.Errorf("a: %+v, %v", tg, ok)
	}
	for host, port := range map[string]int{
		"ui.b.boxes.example.com":  80,
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
	for _, host := range []string{"b.boxes.example.com", "x.b.boxes.example.com", "web.gone.boxes.example.com"} {
		if _, ok := s.lookup(host); ok {
			t.Errorf("served %s", host)
		}
	}

	if err := SaveRoute(routes, "a", nil); err != nil {
		t.Fatal(err)
	}
	s.Reload()
	if _, ok := s.lookup("web.a.boxes.example.com"); ok {
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

// Requests that another host's page starts don't get to use the cookie,
// the box never sees the proxy's cookie, and a box's cookies stay on its
// own host.
func TestCrossHost(t *testing.T) {
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(cookieName); err == nil {
			t.Error("box got the proxy cookie")
		}
		if _, err := r.Cookie(oldCookie); err == nil {
			t.Error("box got the old proxy cookie")
		}
		w.Header().Set("Origin-Agent-Cluster", "?0")
		w.Header().Add("Set-Cookie", "s=1; Path=/; Domain=boxes.example.com; HttpOnly")
		w.Header().Add("Set-Cookie", "t=2; domain=.example.com; Domain =x.example.com")
	}))
	defer box.Close()
	ap := netip.MustParseAddrPort(strings.TrimPrefix(box.URL, "http://"))
	sum := sha256.Sum256([]byte("tok"))
	s := &Server{Domain: "boxes.example.com", routes: map[string]target{
		"pi.b.boxes.example.com": {IP: ap.Addr(), Port: int(ap.Port()), TokenHash: hex.EncodeToString(sum[:])},
	}}

	for _, c := range []struct {
		name, method string
		hdr          map[string]string
		headerAuth   bool
		code         int
	}{
		{"own page", "POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}, false, 200},
		{"typed in", "GET", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, false, 200},
		{"link from a sibling", "GET", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, false, 200},
		{"frame on a sibling", "GET", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}, false, 403},
		{"fetch from a sibling", "GET", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors"}, false, 403},
		{"websocket from a sibling", "GET", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "websocket", "Origin": "https://evil.boxes.example.com"}, false, 403},
		{"form post from a sibling", "POST", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate"}, false, 403},
		{"old browser, sibling origin", "POST", map[string]string{"Origin": "https://evil.boxes.example.com"}, false, 403},
		{"old browser, own origin", "POST", map[string]string{"Origin": "https://pi.b.boxes.example.com"}, false, 200},
		{"not a browser", "POST", nil, false, 200},
		{"basic auth from a sibling", "GET", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "websocket", "Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("x:tok"))}, true, 403},
		{"bearer from anywhere", "POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://x.example"}, true, 200},
	} {
		r := httptest.NewRequest(c.method, "https://pi.b.boxes.example.com/ws", nil)
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if c.headerAuth {
			if r.Header.Get("Authorization") == "" {
				r.Header.Set("Authorization", "Bearer tok")
			}
		} else {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: "tok"})
		}
		r.AddCookie(&http.Cookie{Name: oldCookie, Value: "tok"})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != c.code {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.code)
		}
		if got := w.Header().Values("Origin-Agent-Cluster"); len(got) != 1 || got[0] != "?1" {
			t.Errorf("%s: Origin-Agent-Cluster %q", c.name, got)
		}
		if c.code != 200 {
			continue
		}
		want := []string{"s=1; Path=/; HttpOnly", "t=2"}
		if got := w.Header().Values("Set-Cookie"); !slices.Equal(got, want) {
			t.Errorf("%s: Set-Cookie %q, want %q", c.name, got, want)
		}
		if !slices.Contains(w.Header().Values("Vary"), "Sec-Fetch-Site") {
			t.Errorf("%s: no Vary: Sec-Fetch-Site", c.name)
		}
	}
}

func TestLoginCookie(t *testing.T) {
	sum := sha256.Sum256([]byte("tok"))
	s := &Server{Domain: "boxes.example.com", routes: map[string]target{
		"pi.b.boxes.example.com": {TokenHash: hex.EncodeToString(sum[:])},
	}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "https://pi.b.boxes.example.com"+loginPath+"?token=tok", nil))
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != cookieName || !c[0].Secure || c[0].Domain != "" || c[0].Path != "/" {
		t.Errorf("login cookie %+v", c)
	}
}
