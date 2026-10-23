// Package proxy is the HTTP front door. It is closed by default: a request
// reaches a box only if its Host is <box>.<root domain> for a box exposed
// with `cyac expose`, and, unless that route is public, it carries the
// route's token.
package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containmentzone/cyacrt/internal/box"
)

const (
	cookieName = "cyac_token"
	loginPath  = "/__cyac/login"
)

type target struct {
	box.Route
	IP netip.Addr
}

// transport reaches boxes on the bridge. Boxes are local, so connecting is
// quick or not happening; a box gets a minute to start answering, and
// once it does, bodies (uploads, streams, websockets) take as long as
// they take.
var transport = &http.Transport{
	Proxy:                 nil, // never an outbound proxy from the environment
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ResponseHeaderTimeout: time.Minute,
	ExpectContinueTimeout: time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   16,
}

type Server struct {
	Store box.Store
	// Domain is the root domain; box b answers at <b>.<Domain>.
	Domain string

	mu     sync.RWMutex
	routes map[string]target // by lowercased host
}

// Reload rebuilds the route table from the store. Call it periodically or
// on SIGHUP; the CLI writes records, the proxy only reads them.
func (s *Server) Reload() error {
	boxes, err := s.Store.List()
	if err != nil {
		return err
	}
	routes := map[string]target{}
	for _, b := range boxes {
		if b.Route != nil {
			routes[b.Name+"."+strings.ToLower(s.Domain)] = target{Route: *b.Route, IP: s.Store.IP(b)}
		}
	}
	s.mu.Lock()
	s.routes = routes
	s.mu.Unlock()
	return nil
}

func (s *Server) lookup(host string) (target, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.routes[strings.ToLower(host)]
	return t, ok
}

// HostPolicy only lets ACME issue certificates for exposed hosts.
func (s *Server) HostPolicy(_ context.Context, host string) error {
	if _, ok := s.lookup(host); !ok {
		return errors.New("unknown host")
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lookup(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if r.URL.Path == loginPath {
		s.login(w, r, t)
		return
	}

	tok, fromHeader := tokenFrom(r)
	if !t.Public && !validToken(t.Route, tok) {
		w.Header().Set("WWW-Authenticate", `Basic realm="cyac"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rp := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort(t.IP.String(), strconv.Itoa(t.Port))})
			pr.Out.Host = pr.In.Host // apps see the public hostname
			pr.SetXForwarded()
			for k := range pr.Out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Cyac-") {
					pr.Out.Header.Del(k)
				}
			}
			// Our credentials are for the proxy, not the box.
			if fromHeader {
				pr.Out.Header.Del("Authorization")
			}
			stripCookie(pr.Out, cookieName)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy %s: %v", r.Host, err)
			http.Error(w, "box unavailable", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// login trades ?token= for a cookie so browsers don't need basic auth.
func (s *Server) login(w http.ResponseWriter, r *http.Request, t target) {
	tok := r.URL.Query().Get("token")
	if !t.Public && !validToken(t.Route, tok) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// tokenFrom accepts a bearer token, a basic-auth password, or the cookie.
// fromHeader reports whether it came from the Authorization header.
func tokenFrom(r *http.Request) (tok string, fromHeader bool) {
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return v, true
	}
	if _, pw, ok := r.BasicAuth(); ok {
		return pw, true
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value, false
	}
	return "", false
}

func validToken(rt box.Route, tok string) bool {
	if tok == "" || rt.TokenHash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(rt.TokenHash)) == 1
}

func stripCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}
