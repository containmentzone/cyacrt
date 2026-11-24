// Package proxy is the HTTP front door. It is closed by default: a request
// reaches a box only if its Host is one of the hosts of a box exposed with
// `cyac expose` (see Hosts), and, unless that port is public, it carries
// the box's token. The root domain itself gets a splash page.
package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containmentzone/cyacrt/internal/conf"
)

const (
	cookieName = "cyac_token"
	loginPath  = "/__cyac/login"
)

// splash is what the bare root domain shows.
//
//go:embed splash.html
var splash []byte

// Route exposes a box's named ports. Nothing is reachable without one.
type Route struct {
	// Ports maps names to ports in the box; see Hosts.
	Ports map[string]int `json:"ports,omitempty"`
	// Port is the format from before named ports, read as Ports[""].
	Port      int    `json:"port,omitempty"`
	Public    bool   `json:"public,omitempty"`
	TokenHash string `json:"token_hash,omitempty"` // hex sha256 of the bearer token
}

// Hosts returns the hosts box answers at under domain, with their ports:
// <name>.<box>.<domain> for each named port and <box>.<domain> for the
// port named "".
func Hosts(box, domain string, r Route) map[string]int {
	hosts := map[string]int{}
	for name, port := range r.Ports {
		h := box + "." + domain
		if name != "" {
			h = name + "." + h
		}
		hosts[strings.ToLower(h)] = port
	}
	return hosts
}

type target struct {
	IP        netip.Addr
	Port      int
	Public    bool
	TokenHash string
}

// transport reaches boxes on their networks. Boxes are local, so connecting is
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
	Conf   string // conf dir: where boxes and their addresses come from
	Routes string // route dir (see SaveRoute)
	// Domain is the root domain; box b answers at <b>.<Domain>.
	Domain string

	mu     sync.RWMutex
	routes map[string]target // by lowercased host
}

// Reload rebuilds the route table from the routes and the conf. A route
// whose box is gone from the conf is kept on disk but not served.
func (s *Server) Reload() error {
	c, _, err := conf.Load(s.Conf)
	if err != nil {
		return err
	}
	saved, err := LoadRoutes(s.Routes)
	if err != nil {
		return err
	}
	routes := map[string]target{}
	for name, r := range saved {
		b := c.Boxes[name]
		if b == nil {
			continue
		}
		for host, port := range Hosts(name, s.Domain, r) {
			routes[host] = target{IP: b.Address, Port: port, Public: r.Public, TokenHash: r.TokenHash}
		}
	}
	s.mu.Lock()
	s.routes = routes
	s.mu.Unlock()
	return nil
}

// LoadRoutes reads the routes in dir, one <box>.json each.
func LoadRoutes(dir string) (map[string]Route, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	routes := map[string]Route{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var r Route
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if r.Ports == nil && r.Port != 0 {
			r.Ports, r.Port = map[string]int{"": r.Port}, 0
		}
		routes[strings.TrimSuffix(filepath.Base(f), ".json")] = r
	}
	return routes, nil
}

// SaveRoute stores box name's route in dir, or removes it if r is nil.
func SaveRoute(dir, name string, r *Route) error {
	if !conf.ValidName(name) {
		return fmt.Errorf("invalid box name %q", name)
	}
	path := filepath.Join(dir, name+".json")
	if r == nil {
		return os.Remove(path)
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func hostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

func (s *Server) isRoot(host string) bool {
	return hostname(host) == strings.ToLower(s.Domain)
}

func (s *Server) lookup(host string) (target, bool) {
	host = hostname(host)
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.routes[host]
	return t, ok
}

// HostPolicy only lets ACME issue certificates for the root domain and
// exposed hosts.
func (s *Server) HostPolicy(_ context.Context, host string) error {
	if _, ok := s.lookup(host); !ok && !s.isRoot(host) {
		return errors.New("unknown host")
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.isRoot(r.Host) && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(splash)
		return
	}
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
	if !t.Public && !validToken(t.TokenHash, tok) {
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
	if !t.Public && !validToken(t.TokenHash, tok) {
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

func validToken(hash, tok string) bool {
	if tok == "" || hash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(hash)) == 1
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
