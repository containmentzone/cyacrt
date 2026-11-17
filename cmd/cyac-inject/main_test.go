package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandler(t *testing.T) {
	var got *http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		io.WriteString(w, "ok")
	}))
	defer up.Close()

	h, err := handler(up.URL+"/api/v1", "authorization", "Bearer sekrit")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	req, _ := http.NewRequest("POST", proxy.URL+"/chat/completions?x=1", nil)
	req.Header.Set("Authorization", "Bearer from-the-box")
	req.Header.Set("X-Forwarded-For", "10.42.0.10")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body %q", body)
	}
	if got.URL.Path != "/api/v1/chat/completions" || got.URL.RawQuery != "x=1" {
		t.Errorf("upstream got %s", got.URL)
	}
	if a := got.Header.Values("Authorization"); len(a) != 1 || a[0] != "Bearer sekrit" {
		t.Errorf("Authorization %q", a)
	}
	if got.Host != up.Listener.Addr().String() {
		t.Errorf("Host %q, want the upstream's", got.Host)
	}
	if f := got.Header.Get("X-Forwarded-For"); f != "" {
		t.Errorf("X-Forwarded-For %q leaked", f)
	}
}

func TestHandlerBadUpstream(t *testing.T) {
	for _, u := range []string{"", "nano-gpt.com", "ftp://x", "https://"} {
		if _, err := handler(u, "Authorization", "v"); err == nil {
			t.Errorf("%q: no error", u)
		}
	}
}

func TestReadValue(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]string{"key\n": "key", "key": "key", "key\r\n": "key", "": "", "a\nb\n": "", "\n": ""} {
		p := filepath.Join(dir, "v")
		os.WriteFile(p, []byte(content), 0o600)
		v, err := readValue(p)
		if v != want || (err != nil) != (want == "") {
			t.Errorf("%q: got %q, %v", content, v, err)
		}
	}
	if _, err := readValue(""); err == nil {
		t.Error("no path: no error")
	}
}
