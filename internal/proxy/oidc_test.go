package proxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/containmentzone/cyacrt/internal/box"
)

// fakeIdP signs in everyone as sub "user1" with the given email.
func fakeIdP(t *testing.T, email string, verified bool) *httptest.Server {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k"}}, nil)
	var nonce string
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer": idp.URL, "authorization_endpoint": idp.URL + "/auth", "token_endpoint": idp.URL + "/token",
				"jwks_uri": idp.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/jwks":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k", Algorithm: "RS256"}}})
		case "/auth":
			q := r.URL.Query()
			if q.Get("code_challenge_method") != "S256" {
				t.Error("no PKCE challenge")
			}
			nonce = q.Get("nonce")
			http.Redirect(w, r, q.Get("redirect_uri")+"?code=c&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
		case "/token":
			if r.FormValue("code_verifier") == "" {
				t.Error("no PKCE verifier")
			}
			tok, _ := jwt.Signed(signer).Claims(map[string]any{
				"iss": idp.URL, "aud": "cid", "sub": "user1", "nonce": nonce,
				"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
				"email": email, "email_verified": verified,
			}).Serialize()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": tok})
		}
	}))
	t.Cleanup(idp.Close)
	return idp
}

// setup runs a proxy for web.boxes.test and other.boxes.test with
// auth.test as the OIDC host, and a client that resolves *.test to it.
func setup(t *testing.T, idp *httptest.Server) (*Server, *http.Client, string) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Header.Get("X-Cyac-User")+" "+r.Header.Get("X-Cyac-Email")+" "+r.Header.Get("Cookie"))
	}))
	t.Cleanup(upstream.Close)
	addr := netip.MustParseAddrPort(upstream.Listener.Addr().String())
	route := target{Route: box.Route{Port: int(addr.Port())}, IP: addr.Addr()}

	s := &Server{routes: map[string]target{"web.boxes.test": route, "other.boxes.test": route}}
	front := httptest.NewServer(s)
	t.Cleanup(front.Close)
	port := strconv.Itoa(front.Listener.Addr().(*net.TCPAddr).Port)

	var err error
	s.OIDC, err = NewOIDC(context.Background(), idp.URL, "cid", "secret",
		"http://auth.test:"+port+"/__cyac/oidc/callback", []string{"A@example.com"}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
			if strings.HasSuffix(hostname(a), ".test") {
				a = front.Listener.Addr().String()
			}
			return (&net.Dialer{}).DialContext(ctx, network, a)
		},
	}}
	return s, c, port
}

func get(t *testing.T, c *http.Client, u string, cookies ...*http.Cookie) (int, string) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestOIDCSignIn(t *testing.T) {
	_, c, port := setup(t, fakeIdP(t, "a@Example.com", true))
	u, _ := url.Parse("http://web.boxes.test:" + port)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "app", Value: "1"}})
	code, body := get(t, c, u.String()+"/x?y=1")
	if code != 200 || body != "user1 a@example.com app=1" {
		t.Fatalf("got %d %q", code, body)
	}

	// The session is for web.boxes.test only.
	var sess *http.Cookie
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == sessionCookie {
			sess = ck
		}
	}
	noRedirect := &http.Client{Transport: c.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if code, _ := get(t, noRedirect, "http://other.boxes.test:"+port+"/", sess); code != http.StatusFound {
		t.Fatalf("session replayed on another host: got %d", code)
	}
}

func TestOIDCRejects(t *testing.T) {
	for name, idp := range map[string]*httptest.Server{
		"unverified email": fakeIdP(t, "a@example.com", false),
		"unlisted email":   fakeIdP(t, "b@example.com", true),
	} {
		_, c, port := setup(t, idp)
		if code, _ := get(t, c, "http://web.boxes.test:"+port+"/"); code != http.StatusForbidden {
			t.Errorf("%s: got %d", name, code)
		}
	}

	_, c, port := setup(t, fakeIdP(t, "a@example.com", true))
	for name, u := range map[string]string{
		"bad ticket":   "http://web.boxes.test:" + port + "/__cyac/oidc/finish?t=garbage",
		"open rd":      "http://auth.test:" + port + "/__cyac/oidc/login?rd=" + url.QueryEscape("http://evil.example/"),
		"no flow":      "http://auth.test:" + port + "/__cyac/oidc/callback?code=c&state=s",
		"unknown host": "http://nope.boxes.test:" + port + "/",
	} {
		if code, _ := get(t, c, u); code < 400 {
			t.Errorf("%s: got %d", name, code)
		}
	}
}
