// Package server is the cyac daemon's API. Boxes are what the conf
// defines and their units run; the server only looks at them, reaches
// into them (exec, console), asks systemd to start or stop them, and
// keeps the proxy's routes. See package api for the endpoints.
package server

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/net/idna"

	"github.com/containmentzone/cyacrt/internal/api"
	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/lease"
	"github.com/containmentzone/cyacrt/internal/proxy"
	"github.com/containmentzone/cyacrt/internal/run"
	"github.com/containmentzone/cyacrt/internal/sh"
	"github.com/containmentzone/cyacrt/internal/units"
)

type Server struct {
	Conf   string      // conf dir; boxes are what it defines
	Leases string      // lease dir, for box addresses (see package lease)
	Run    *run.Runner // to see which boxes run, and reach them
	// Routes is where exposed routes are kept (see proxy.SaveRoute).
	Routes string
	// Domain is the root domain: port p of box b answers at <p>.<b>.<Domain>.
	Domain string
	// RoutesChanged is called after every route change (the proxy's Reload).
	RoutesChanged func() error

	mu sync.Mutex // serializes route changes
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/boxes", s.list)
	mux.HandleFunc("POST /v1/boxes/{name}/start", s.unit("start"))
	mux.HandleFunc("POST /v1/boxes/{name}/stop", s.unit("stop"))
	mux.HandleFunc("PUT /v1/boxes/{name}/route", s.expose)
	mux.HandleFunc("DELETE /v1/boxes/{name}/route", s.unexpose)
	mux.HandleFunc("POST /v1/boxes/{name}/route/regen", s.regen)
	mux.HandleFunc("POST /v1/boxes/{name}/exec", s.exec)
	mux.HandleFunc("POST /v1/boxes/{name}/console", s.console)
	return logRequests(mux)
}

// statusWriter records the status code for logRequests. Unwrap keeps
// Hijack and Flush reachable through http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack is only used by exec, which then answers 101 itself.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.status = http.StatusSwitchingProtocols
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		slog.Info("api", "method", r.Method, "path", r.URL.Path, "status", sw.status, "took", time.Since(start).Round(time.Millisecond))
	})
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		v = api.Error{Error: err.Error()}
	}
	json.NewEncoder(w).Encode(v)
}

// box returns the box the conf defines as name.
func (s *Server) box(name string) (*conf.Box, error) {
	c, probs, err := conf.Load(s.Conf)
	if err != nil {
		return nil, err
	}
	for _, p := range probs {
		if p.Kind == conf.KindBox && p.Name == name {
			return nil, fmt.Errorf("%s: %w", conf.Path(s.Conf, p.Kind, p.Name), p.Err)
		}
	}
	if b := c.Boxes[name]; b != nil {
		return b, nil
	}
	return nil, fmt.Errorf("no box %q (no %s)", name, conf.Path(s.Conf, conf.KindBox, name))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	c, _, err := conf.Load(s.Conf)
	if err != nil {
		reply(w, nil, err)
		return
	}
	if err := lease.Read(s.Leases, c); err != nil {
		reply(w, nil, err)
		return
	}
	routes, err := proxy.LoadRoutes(s.Routes)
	if err != nil {
		reply(w, nil, err)
		return
	}
	out := []api.Box{}
	for _, name := range conf.Sorted(c.Boxes) {
		b := c.Boxes[name]
		v := api.Box{Name: name, Image: b.Workload.Image, Network: b.Network, Status: "stopped"}
		if b.Address.IsValid() {
			v.IP = b.Address.String()
		}
		if rt, _ := s.Run.Running(conf.KindBox, name); rt != nil {
			v.Status = "running (" + rt.Name() + ")"
		}
		if rt, ok := routes[name]; ok {
			v.Route = &api.Route{Hosts: proxy.Hosts(name, s.Domain, rt)}
		}
		out = append(out, v)
	}
	reply(w, out, nil)
}

// unit starts or stops the box's unit; systemd does the rest.
func (s *Server) unit(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := s.box(name); err != nil {
			reply(w, nil, err)
			return
		}
		reply(w, struct{}{}, sh.Run("systemctl", verb, units.Name(conf.KindBox, name)))
	}
}

func (s *Server) expose(w http.ResponseWriter, r *http.Request) {
	var req api.Expose
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, nil, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	route, err := s.addRoute(name, req)
	reply(w, s.exposed(name, route), err)
}

// addRoute exposes the image's default ports and req's on top, replacing
// any earlier route. Names it had before keep their tokens.
func (s *Server) addRoute(name string, req api.Expose) (*proxy.Route, error) {
	if _, err := s.box(name); err != nil {
		return nil, err
	}
	ports := map[string]int{}
	if img := s.Run.Image(conf.KindBox, name); img != nil {
		maps.Copy(ports, img.Expose)
	}
	maps.Copy(ports, req.Ports)
	if len(ports) == 0 {
		return nil, errors.New("no ports given, and the image exposes none")
	}
	for n, p := range ports {
		if !conf.ValidName(n) {
			return nil, fmt.Errorf("invalid port name %q", n)
		}
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("%s: need a port between 1 and 65535", n)
		}
	}
	// autocert only serves hosts that pass the same check (no "--" in
	// positions 3 and 4, no trailing hyphen).
	for h := range proxy.Hosts(name, s.Domain, proxy.Route{Ports: ports}) {
		if _, err := idna.Lookup.ToASCII(h); err != nil {
			return nil, fmt.Errorf("%s can't get a certificate: %w", h, err)
		}
	}
	routes, err := proxy.LoadRoutes(s.Routes)
	if err != nil {
		return nil, err
	}
	route := &proxy.Route{Ports: ports, Tokens: map[string]string{}}
	for n := range ports {
		if tok := routes[name].Tokens[n]; tok != "" {
			route.Tokens[n] = tok
		} else {
			route.Tokens[n] = rand.Text()
		}
	}
	if err := proxy.SaveRoute(s.Routes, name, route); err != nil {
		return nil, err
	}
	slog.Info("exposed route", "box", name, "ports", ports)
	return route, s.RoutesChanged()
}

// regen gives the named ports of an exposed box new tokens (all of
// them if none are named), which logs out everyone using the old ones.
func (s *Server) regen(w http.ResponseWriter, r *http.Request) {
	var req api.Regen
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, nil, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	route, err := s.regenTokens(name, req.Names)
	reply(w, s.exposed(name, route), err)
}

func (s *Server) regenTokens(name string, names []string) (*proxy.Route, error) {
	routes, err := proxy.LoadRoutes(s.Routes)
	if err != nil {
		return nil, err
	}
	route, ok := routes[name]
	if !ok {
		return nil, fmt.Errorf("%s is not exposed", name)
	}
	if len(names) == 0 {
		names = slices.Collect(maps.Keys(route.Ports))
	}
	if route.Tokens == nil {
		route.Tokens = map[string]string{}
	}
	for _, n := range names {
		if _, ok := route.Ports[n]; !ok {
			return nil, fmt.Errorf("%s has no port named %q", name, n)
		}
		route.Tokens[n] = rand.Text()
	}
	if err := proxy.SaveRoute(s.Routes, name, &route); err != nil {
		return nil, err
	}
	slog.Info("regenerated tokens", "box", name, "ports", names)
	return &route, s.RoutesChanged()
}

// exposed is what expose and regen answer: every host with its token.
func (s *Server) exposed(name string, r *proxy.Route) *api.Exposed {
	if r == nil {
		return nil
	}
	e := &api.Exposed{Hosts: map[string]api.Host{}}
	for n, port := range r.Ports {
		e.Hosts[proxy.Host(n, name, s.Domain)] = api.Host{Port: port, Token: r.Tokens[n]}
	}
	return e
}

func (s *Server) unexpose(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	routes, err := proxy.LoadRoutes(s.Routes)
	if err == nil {
		if _, ok := routes[name]; !ok {
			err = fmt.Errorf("%s is not exposed", name)
		}
	}
	if err == nil {
		err = proxy.SaveRoute(s.Routes, name, nil)
	}
	if err == nil {
		slog.Info("removed route", "box", name)
		err = s.RoutesChanged()
	}
	reply(w, struct{}{}, err)
}

// command returns a command that runs args inside the running box; start
// it with a terminal on its stdio. It runs as the image's login user, if
// it named one, unless root is set, with TERM set to term if given.
func (s *Server) command(name string, args []string, root bool, term string) (*exec.Cmd, error) {
	if _, err := s.box(name); err != nil {
		return nil, err
	}
	rt, m := s.Run.Running(conf.KindBox, name)
	if rt == nil {
		return nil, fmt.Errorf("%s is not running", name)
	}
	if img := s.Run.Image(conf.KindBox, name); img != nil && img.User != "" && !root {
		// A login shell, so the user's HOME, profile and MOTD apply. With
		// -c, runuser starts a new session, which would leave the shell
		// without a controlling terminal (no job control); --pty gives it one.
		args = []string{"runuser", "--pty", "-l", img.User, "-c", sh.Quote(args)}
	}
	if term != "" {
		// runuser -l keeps TERM.
		args = append([]string{"env", "TERM=" + term}, args...)
	}
	return rt.Command(m, args)
}

// exec runs a command on a pty in the box, then switches the connection to
// raw terminal bytes in both directions until the command exits.
// TODO: window resizes after start, exit status.
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req api.Exec
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, nil, err)
		return
	}
	name := r.PathValue("name")
	cmd, err := s.command(name, req.Args, req.Root, req.Term)
	if err != nil {
		reply(w, nil, err)
		return
	}
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
	if err != nil {
		reply(w, nil, err)
		return
	}
	defer tty.Close()
	slog.Info("exec started", "box", name, "args", req.Args, "root", req.Root, "pid", cmd.Process.Pid)

	conn, buf, err := upgrade(w, api.ExecUpgrade)
	if err != nil {
		cmd.Process.Kill()
		return
	}
	defer conn.Close()

	go io.Copy(tty, buf) // client keystrokes; ends when the conn closes
	io.Copy(conn, tty)   // command output; ends when the command exits
	err = cmd.Wait()
	slog.Info("exec ended", "box", name, "pid", cmd.Process.Pid, "err", err)
}

// upgrade takes over the connection and answers 101 with token.
func upgrade(w http.ResponseWriter, token string) (net.Conn, *bufio.ReadWriter, error) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", token)
	return conn, buf, buf.Flush()
}

// console streams the tail of the box's console log, then follows it, and
// writes what the client types into the console's input FIFO. When
// nothing reads that FIFO (a stopped box, or an init like systemd under
// runc that detaches from its stdio) it is output only.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.box(name); err != nil {
		reply(w, nil, err)
		return
	}
	logPath, inPath := s.Run.Console(conf.KindBox, name)
	log, err := os.Open(logPath)
	if err != nil {
		reply(w, nil, fmt.Errorf("%s has no console output yet", name))
		return
	}
	defer log.Close()
	if fi, err := log.Stat(); err == nil {
		log.Seek(max(0, fi.Size()-16<<10), io.SeekStart)
	}
	// Non-blocking, so this fails instead of hanging when nobody reads the
	// FIFO or it doesn't exist.
	var in io.Writer = io.Discard
	if f, err := os.OpenFile(inPath, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
		defer f.Close()
		in = f
	}

	io.Copy(io.Discard, r.Body) // so a request body isn't taken for keystrokes
	conn, buf, err := upgrade(w, api.ConsoleUpgrade)
	if err != nil {
		return
	}
	defer conn.Close()
	if in == io.Discard {
		fmt.Fprintf(conn, "[output only: %s is stopped, or its init doesn't read the console]\r\n", name)
	}
	slog.Info("console attached", "box", name)
	defer slog.Info("console detached", "box", name)

	done := make(chan struct{})
	go func() {
		io.Copy(in, buf) // ends when the client detaches
		close(done)
	}()
	p := make([]byte, 32<<10)
	for {
		n, err := log.Read(p)
		if n > 0 {
			if _, err := conn.Write(p[:n]); err != nil {
				return
			}
		}
		if err == io.EOF {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
		} else if err != nil {
			return
		}
	}
}
