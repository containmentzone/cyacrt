// Package server is the cyac daemon's API: it owns the store, serializes
// changes to it, and tells the proxy when routes change. See package api
// for the endpoints.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/creack/pty"

	"github.com/containmentzone/cyacrt/internal/api"
	"github.com/containmentzone/cyacrt/internal/box"
)

type Server struct {
	Store box.Store
	// RoutesChanged is called after every route change (the proxy's Reload).
	RoutesChanged func() error

	mu sync.Mutex // serializes everything that changes the store
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/boxes", s.list)
	mux.HandleFunc("POST /v1/boxes", s.create)
	mux.HandleFunc("POST /v1/boxes/{name}/start", s.withBox(s.Store.Start))
	mux.HandleFunc("POST /v1/boxes/{name}/stop", s.withBox(s.Store.Stop))
	mux.HandleFunc("DELETE /v1/boxes/{name}", s.withBox(s.remove))
	mux.HandleFunc("POST /v1/boxes/{name}/resize", s.resize)
	mux.HandleFunc("POST /v1/boxes/{name}/routes", s.expose)
	mux.HandleFunc("DELETE /v1/routes/{host}", s.unexpose)
	mux.HandleFunc("POST /v1/boxes/{name}/exec", s.exec)
	return mux
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		v = api.Error{Error: err.Error()}
	}
	json.NewEncoder(w).Encode(v)
}

func (s *Server) view(b *box.Box) api.Box {
	out := api.Box{Name: b.Name, Image: b.Image, IP: s.Store.IP(b).String(), Status: s.Store.Status(b)}
	for _, r := range b.Routes {
		out.Routes = append(out.Routes, api.Route{Host: r.Host, Port: r.Port, Public: r.Public})
	}
	return out
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	boxes, err := s.Store.List()
	out := []api.Box{}
	for _, b := range boxes {
		out = append(out, s.view(b))
	}
	reply(w, out, err)
}

// withBox runs a lifecycle action on the named box under the lock.
func (s *Server) withBox(action func(*box.Box) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		b, err := s.Store.Get(r.PathValue("name"))
		if err == nil {
			err = action(b)
		}
		reply(w, struct{}{}, err)
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req api.NewBox
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, nil, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.newBox(req)
	if err != nil {
		reply(w, nil, err)
		return
	}
	reply(w, s.view(b), nil)
}

func (s *Server) newBox(req api.NewBox) (*box.Box, error) {
	switch {
	case !box.ValidName(req.Name):
		return nil, fmt.Errorf("invalid box name %q (lowercase letters, digits, dashes)", req.Name)
	case req.Image == "":
		return nil, errors.New("image is required")
	case req.SizeBytes <= 0 || req.CPUs <= 0 || req.MemMiB <= 0:
		return nil, errors.New("size, cpus and mem must be positive")
	}
	if _, err := s.Store.Get(req.Name); err == nil {
		return nil, fmt.Errorf("box %q already exists", req.Name)
	}
	id, err := s.Store.FreeID()
	if err != nil {
		return nil, err
	}
	b := &box.Box{Name: req.Name, Image: req.Image, ID: id, CPUs: req.CPUs, MemMiB: req.MemMiB, Init: req.Init}
	log.Printf("seeding %s from %s", b.Name, b.Image)
	if err := s.Store.Seed(b, req.SizeBytes); err != nil {
		return nil, err
	}
	if err := s.Store.Put(b); err != nil {
		return nil, err
	}
	return b, s.Store.Start(b)
}

func (s *Server) remove(b *box.Box) error {
	if err := s.Store.Remove(b); err != nil {
		return err
	}
	return s.RoutesChanged()
}

func (s *Server) resize(w http.ResponseWriter, r *http.Request) {
	var req api.Resize
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, nil, err)
		return
	}
	s.withBox(func(b *box.Box) error { return s.Store.Resize(b, req.SizeBytes) })(w, r)
}

func (s *Server) expose(w http.ResponseWriter, r *http.Request) {
	var req api.Expose
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, nil, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, err := s.addRoute(r.PathValue("name"), req)
	reply(w, api.Token{Token: tok}, err)
}

func (s *Server) addRoute(name string, req api.Expose) (token string, err error) {
	b, err := s.Store.Get(name)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(req.Host)
	if req.Port < 1 || req.Port > 65535 || host == "" {
		return "", errors.New("need a host and a port between 1 and 65535")
	}
	boxes, err := s.Store.List()
	if err != nil {
		return "", err
	}
	for _, other := range boxes {
		for _, rt := range other.Routes {
			if rt.Host == host {
				return "", fmt.Errorf("%s is already exposed by %s", host, other.Name)
			}
		}
	}
	route := box.Route{Host: host, Port: req.Port, Public: req.Public}
	if !req.Public {
		token = rand.Text()
		sum := sha256.Sum256([]byte(token))
		route.TokenHash = hex.EncodeToString(sum[:])
	}
	b.Routes = append(b.Routes, route)
	if err := s.Store.Put(b); err != nil {
		return "", err
	}
	return token, s.RoutesChanged()
}

func (s *Server) unexpose(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply(w, struct{}{}, s.removeRoute(strings.ToLower(r.PathValue("host"))))
}

func (s *Server) removeRoute(host string) error {
	boxes, err := s.Store.List()
	if err != nil {
		return err
	}
	for _, b := range boxes {
		for i, rt := range b.Routes {
			if rt.Host == host {
				b.Routes = append(b.Routes[:i], b.Routes[i+1:]...)
				if err := s.Store.Put(b); err != nil {
					return err
				}
				return s.RoutesChanged()
			}
		}
	}
	return fmt.Errorf("%s is not exposed", host)
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
	b, err := s.Store.Get(r.PathValue("name"))
	if err != nil {
		reply(w, nil, err)
		return
	}
	cmd, err := s.Store.Command(b, req.Args)
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

	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		cmd.Process.Kill()
		return
	}
	defer conn.Close()
	fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", api.ExecUpgrade)
	buf.Flush()

	go io.Copy(tty, buf) // client keystrokes; ends when the conn closes
	io.Copy(conn, tty)   // command output; ends when the command exits
	cmd.Wait()
}
