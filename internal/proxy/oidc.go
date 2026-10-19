package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	oidcPrefix    = "/__cyac/oidc/"
	flowCookie    = "cyac_oidc"    // on the auth host, for one login
	sessionCookie = "cyac_session" // on each route host
	sessionTTL    = 12 * time.Hour
)

// OIDC signs people in with an OpenID Connect provider (authorization code
// flow with PKCE). Every route shares one callback on the redirect URL's
// host, so the provider needs a single redirect URI. The callback hands
// the route's host a one-minute ticket, which it trades for its own
// session cookie. Sessions are HMAC-signed, not stored, and are checked
// against the allowlist on every request.
type OIDC struct {
	conf     oauth2.Config
	verifier *oidc.IDTokenVerifier
	base     string // scheme://host of the redirect URL
	host     string // its hostname
	allow    map[string]bool
	key      []byte
}

// NewOIDC discovers the issuer. allow lists subjects or verified emails;
// it may not be empty, so OIDC never opens a route to everyone.
func NewOIDC(ctx context.Context, issuer, clientID, clientSecret, redirectURL string, allow []string, key []byte) (*OIDC, error) {
	if len(allow) == 0 {
		return nil, errors.New("oidc: no allowed users")
	}
	u, err := url.Parse(redirectURL)
	if err != nil || u.Host == "" || u.Path != oidcPrefix+"callback" {
		return nil, fmt.Errorf("oidc: redirect URL must look like https://auth.example.com%scallback", oidcPrefix)
	}
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	o := &OIDC{
		conf: oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
			Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email"},
		},
		verifier: p.Verifier(&oidc.Config{ClientID: clientID}),
		base:     u.Scheme + "://" + u.Host,
		host:     strings.ToLower(u.Hostname()),
		allow:    map[string]bool{},
		key:      key,
	}
	for _, a := range allow {
		if strings.Contains(a, "@") {
			a = strings.ToLower(a)
		}
		o.allow[a] = true
	}
	return o, nil
}

// sealed is everything the proxy hands the browser: the login flow
// cookie, the ticket, and the session cookie, told apart by Kind.
type sealed struct {
	Kind     string `json:"k"`
	URL      string `json:"u,omitempty"` // where the user was going
	Host     string `json:"h,omitempty"` // route a session is for
	Sub      string `json:"s,omitempty"`
	Email    string `json:"e,omitempty"` // verified emails only
	State    string `json:"st,omitempty"`
	Verifier string `json:"v,omitempty"`
	Exp      int64  `json:"x"`
}

func (o *OIDC) seal(c sealed, ttl time.Duration) string {
	c.Exp = time.Now().Add(ttl).Unix()
	b, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + base64.RawURLEncoding.EncodeToString(o.mac(p))
}

func (o *OIDC) open(s, kind string) (c sealed, ok bool) {
	p, sig, _ := strings.Cut(s, ".")
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, o.mac(p)) {
		return c, false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, false
	}
	return c, c.Kind == kind && time.Now().Unix() < c.Exp
}

func (o *OIDC) mac(p string) []byte {
	m := hmac.New(sha256.New, o.key)
	m.Write([]byte(p))
	return m.Sum(nil)
}

func (o *OIDC) allowed(sub, email string) bool {
	return o.allow[sub] || (email != "" && o.allow[email])
}

// session returns the signed-in user for a route host, if any. It is
// safe to call on a nil *OIDC.
func (o *OIDC) session(r *http.Request, host string) (sealed, bool) {
	if o == nil {
		return sealed{}, false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return sealed{}, false
	}
	s, ok := o.open(c.Value, "session")
	return s, ok && s.Host == strings.ToLower(host) && o.allowed(s.Sub, s.Email)
}

// redirect sends a browser on a route host to the auth host to sign in.
func (o *OIDC) redirect(w http.ResponseWriter, r *http.Request) {
	rd := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	if r.TLS != nil {
		rd.Scheme = "https"
	}
	http.Redirect(w, r, o.base+oidcPrefix+"login?rd="+url.QueryEscape(rd.String()), http.StatusFound)
}

// login runs on the auth host: it binds a state, nonce and PKCE verifier
// to this browser and sends it to the provider.
func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	o := s.OIDC
	rd, err := url.Parse(r.URL.Query().Get("rd"))
	if err != nil || (rd.Scheme != "http" && rd.Scheme != "https") {
		http.Error(w, "bad rd", http.StatusBadRequest)
		return
	}
	if _, ok := s.lookup(rd.Host); !ok {
		http.Error(w, "unknown host", http.StatusBadRequest)
		return
	}
	state, verifier := rand.Text(), oauth2.GenerateVerifier()
	http.SetCookie(w, &http.Cookie{
		Name: flowCookie, Value: o.seal(sealed{Kind: "flow", URL: rd.String(), State: state, Verifier: verifier}, 10*time.Minute),
		Path: oidcPrefix, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, o.conf.AuthCodeURL(state, oidc.Nonce(state), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// callback runs on the auth host: it checks the provider's answer and
// hands a ticket to the route host the user started from.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	o := s.OIDC
	c, err := r.Cookie(flowCookie)
	if err != nil {
		http.Error(w, "no login in progress", http.StatusBadRequest)
		return
	}
	f, ok := o.open(c.Value, "flow")
	if !ok || r.URL.Query().Get("state") != f.State {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Path: oidcPrefix, MaxAge: -1})

	tok, err := o.conf.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		log.Printf("oidc exchange: %v", err)
		http.Error(w, "login failed", http.StatusForbidden)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := o.verifier.Verify(r.Context(), raw)
	if err != nil || idt.Nonce != f.State {
		log.Printf("oidc id token: %v", err)
		http.Error(w, "login failed", http.StatusForbidden)
		return
	}
	var cl struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := idt.Claims(&cl); err != nil {
		http.Error(w, "login failed", http.StatusForbidden)
		return
	}
	email := ""
	if cl.Verified {
		email = strings.ToLower(cl.Email)
	}
	if !o.allowed(idt.Subject, email) {
		log.Printf("oidc: %s (%s) is not allowed", idt.Subject, cl.Email)
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	rd, _ := url.Parse(f.URL)
	t := o.seal(sealed{Kind: "ticket", URL: f.URL, Sub: idt.Subject, Email: email}, time.Minute)
	http.Redirect(w, r, rd.Scheme+"://"+rd.Host+oidcPrefix+"finish?t="+url.QueryEscape(t), http.StatusFound)
}

// finish runs on the route host: it trades a ticket for a session cookie.
func (o *OIDC) finish(w http.ResponseWriter, r *http.Request, t target) {
	c, ok := o.open(r.URL.Query().Get("t"), "ticket")
	rd, err := url.Parse(c.URL)
	if !ok || err != nil || !strings.EqualFold(rd.Host, r.Host) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: o.seal(sealed{Kind: "session", Host: strings.ToLower(t.Host), Sub: c.Sub, Email: c.Email}, sessionTTL),
		Path:  "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, rd.String(), http.StatusFound)
}
