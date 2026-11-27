// Package proxy is the HTTP front door. It is closed by default: a request
// reaches a box only if its Host is one of the hosts of a box exposed with
// `cyac expose` (see Hosts), and it carries that host's token. The root domain itself gets a splash page.
//
// Every host sits under one root domain, so to a browser they are all the
// same site: cookies can be scoped to the root, and SameSite does nothing
// between boxes. The proxy can't make the root a public suffix, so it
// stands in for one where it can: it refuses cookie-authenticated requests
// that another host started (see fromElsewhere), keeps its own cookie on
// one host (__Host-), and keeps boxes' cookies on their own host too.
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/lease"
)

const (
	// cookieName's __Host- prefix makes browsers refuse it with a Domain,
	// so no other host can set or shadow it.
	cookieName = "__Host-cyac_token"
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
	// Tokens maps the same names to their bearer tokens, kept until
	// regenerated (the route file is root's, 0600). A name without one
	// isn't served.
	Tokens map[string]string `json:"tokens,omitempty"`
}

// Hosts returns the hosts box answers at under domain, with their ports:
// <name>.<box>.<domain> for each named port.
func Hosts(box, domain string, r Route) map[string]int {
	hosts := map[string]int{}
	for name, port := range r.Ports {
		hosts[Host(name, box, domain)] = port
	}
	return hosts
}

// Host is where port name of box answers: <name>.<box>.<domain>.
func Host(name, box, domain string) string {
	return strings.ToLower(name + "." + box + "." + domain)
}

type target struct {
	IP        netip.Addr
	Port      int
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
	Conf   string // conf dir: where boxes come from
	Leases string // lease dir: where their addresses come from (see package lease)
	Routes string // route dir (see SaveRoute)
	// Domain is the root domain; port p of box b answers at <p>.<b>.<Domain>.
	Domain string

	reloadMu sync.Mutex // one Reload at a time, so an older read can't publish last
	mu       sync.RWMutex
	routes   map[string]target // by lowercased host
}

// Reload rebuilds the route table from the routes and the conf. A route
// whose box is gone from the conf is kept on disk but not served.
func (s *Server) Reload() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	c, _, err := conf.Load(s.Conf)
	if err != nil {
		return err
	}
	if err := lease.Read(s.Leases, c); err != nil {
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
		for pname, port := range r.Ports {
			tok := r.Tokens[pname]
			if tok == "" {
				continue
			}
			sum := sha256.Sum256([]byte(tok))
			if !b.Address.IsValid() {
				continue // never started, so not reachable either
			}
			routes[Host(pname, name, s.Domain)] = target{IP: b.Address, Port: port, TokenHash: hex.EncodeToString(sum[:])}
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
	// Opt every host out of document.domain and into its own agent cluster.
	w.Header().Set("Origin-Agent-Cluster", "?1")
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
	if !validToken(t.TokenHash, tok) {
		w.Header().Set("WWW-Authenticate", `Basic realm="cyac"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Browsers add the cookie, and remembered basic auth, to requests from
	// any host under the root, box or not; only this host's own pages may
	// use them. A bearer token is never added for you.
	bearer := strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !bearer && fromElsewhere(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
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
			stripCookies(pr.Out, cookieName)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Origin-Agent-Cluster") // ours is already set
			// What we let through depends on fetch metadata, so a browser
			// mustn't hand a sibling what it cached for this host's pages.
			resp.Header.Add("Vary", "Sec-Fetch-Site, Sec-Fetch-Mode, Sec-Fetch-Dest")
			for i, c := range resp.Header["Set-Cookie"] {
				resp.Header["Set-Cookie"][i] = hostOnly(c)
			}
			return nil
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
	if !validToken(t.TokenHash, tok) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/",
		MaxAge:   30 * 24 * 3600, // the token outlives browser restarts; regen revokes it
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
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

// fromElsewhere reports whether a page on another origin started r, such
// as another box's page fetching from or opening a websocket to this one.
// A link or redirect from elsewhere may still open a page here, but not
// inside a frame.
func fromElsewhere(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return false
	case "":
		// Not a browser, or one too old to say: go by Origin, which
		// browsers send with websockets and anything but GET and HEAD.
		o := r.Header.Get("Origin")
		if o == "" {
			return false
		}
		u, err := url.Parse(o)
		return err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, r.Host)
	default:
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead
		return !safe || r.Header.Get("Sec-Fetch-Mode") != "navigate" ||
			r.Header.Get("Sec-Fetch-Dest") != "document"
	}
}

// hostOnly drops the Domain attribute from a Set-Cookie value, so a box
// can't set cookies for its siblings or the root.
func hostOnly(setCookie string) string {
	parts := strings.Split(setCookie, ";")
	kept := parts[:1]
	for _, p := range parts[1:] {
		name, _, _ := strings.Cut(p, "=")
		if !strings.EqualFold(strings.TrimSpace(name), "domain") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ";")
}

func stripCookies(r *http.Request, names ...string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if !slices.Contains(names, c.Name) {
			r.AddCookie(c)
		}
	}
}
