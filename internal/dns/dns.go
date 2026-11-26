// Package dns is a gateway's resolver. It runs inside the gateway's
// netns as an unprivileged user and listens on port 53 of every leg, so
// a box asks its router (.1) and a service asks the gateway end of its
// link; nothing on the host is involved. It answers for <name>.cyac.internal
// from the names each client may see (a box its own network's boxes and
// services, a service those of every network that attaches it), with the
// older <name>.<network>.cyac.internal as an alias, and forwards every
// other question to the gateway's resolver. Forwarded queries leave by
// the gateway's own route (its uplink or its tunnel), like any other
// traffic from the netns.
//
// The resolver can't read the conf (it is root's, and holds more than
// names), so `cyac run start` writes each gateway's zone to a file the
// resolver rereads: a name follows the conf as of the last start of any
// object.
package dns

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/render"
)

// Dir is where zone files live: on /run, world-readable, one per gateway.
const Dir = "/run/cyac-dns"

// DefaultUpstreams are the resolvers for a gateway without DNS=, in the
// order they are tried.
var DefaultUpstreams = []string{"1.1.1.1", "1.0.0.1"}

// inFlight caps the queries (UDP) and connections (TCP) served at once;
// more are dropped, so a box that floods the resolver gets timeouts, not
// the host's memory.
const inFlight = 256

// Zone is what one gateway's resolver knows.
type Zone struct {
	Upstreams []string `json:"upstreams"`
	Clients   []Client `json:"clients"`
}

// Client is who may ask, by source address (which the bridge pins), and
// the names they see, fully qualified and lower case. Anyone else is
// refused.
type Client struct {
	From  netip.Prefix          `json:"from"`
	Names map[string]netip.Addr `json:"names"`
}

// ZoneFor builds g's zone from the conf: every network behind g sees its
// own boxes and services, and every service behind g sees the names of
// all the networks that attach it. A service that shares several of
// those networks with another resolves, under the flat name, to its
// address in the first of them (in name order); the alias with the
// network is exact.
func ZoneFor(c *conf.Config, g *conf.Gateway) Zone {
	z := Zone{Upstreams: DefaultUpstreams}
	if g.DNS != "" {
		z.Upstreams = []string{g.DNS}
	}
	names := func(n *conf.Network, into map[string]netip.Addr) {
		add := func(name string, addr netip.Addr) {
			if _, ok := into[name+"."+zone]; !ok {
				into[name+"."+zone] = addr
			}
			into[name+"."+n.Name+"."+zone] = addr // pre-flattening alias
		}
		for _, name := range conf.Sorted(c.Boxes) {
			if b := c.Boxes[name]; b.Network == n.Name {
				add(name, b.Address)
			}
		}
		for _, name := range n.Services {
			add(name, c.Services[name].AddressIn(n))
		}
	}
	for _, name := range conf.Sorted(c.Networks) {
		if n := c.Networks[name]; n.Gateway == g.Name {
			cl := Client{From: n.Prefix(), Names: map[string]netip.Addr{}}
			names(n, cl.Names)
			z.Clients = append(z.Clients, cl)
		}
	}
	for _, name := range conf.Sorted(c.Services) {
		if s := c.Services[name]; s.Gateway == g.Name {
			_, end := s.Link()
			cl := Client{From: netip.PrefixFrom(end.Addr(), 32), Names: map[string]netip.Addr{}}
			for _, n := range c.AttachedBy(name) {
				names(c.Networks[n], cl.Names)
			}
			z.Clients = append(z.Clients, cl)
		}
	}
	return z
}

// Path is gateway gw's zone file in dir.
func Path(dir, gw string) string { return filepath.Join(dir, gw+".json") }

// Write puts every gateway's zone file in dir.
func Write(c *conf.Config, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range conf.Sorted(c.Gateways) {
		data, err := json.Marshal(ZoneFor(c, c.Gateways[name]))
		if err != nil {
			return err
		}
		if err := publish(Path(dir, name), data); err != nil {
			return err
		}
	}
	return nil
}

// publish replaces path with data through a temporary file of its own,
// so starts that overlap (every box at boot) never rename each other's.
func publish(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails harmlessly once renamed
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Forward sends a raw query to the first of upstreams that answers and
// returns the raw reply.
type Forward func(ctx context.Context, upstreams []string, query []byte) ([]byte, error)

// Server answers for one gateway. It rereads the zone file at most once
// a second.
type Server struct {
	File    string
	Forward Forward

	mu     sync.Mutex
	loaded time.Time
	zone   Zone

	logged time.Time // last failure logged
	cache  cache
}

// logf logs a failure to the journal, at most once a second, so a box
// asking in a loop can't flood it.
func (s *Server) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.logged) < time.Second {
		return
	}
	s.logged = time.Now()
	log.Printf(format, args...)
}

// load returns the zone, refreshed if it is more than a second old. A
// missing or broken file is an empty zone: everyone is refused.
func (s *Server) load() Zone {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.loaded) < time.Second {
		return s.zone
	}
	s.loaded, s.zone = time.Now(), Zone{}
	if data, err := os.ReadFile(s.File); err == nil {
		if json.Unmarshal(data, &s.zone) != nil {
			s.zone = Zone{}
		}
	}
	return s.zone
}

// zone is render.Domain as a fully qualified name.
const zone = render.Domain + "."

// Answer returns the reply to one query from from, or nil to drop it.
func (s *Server) Answer(ctx context.Context, from netip.Addr, query []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return reply(h, nil, dnsmessage.RCodeFormatError, nil)
	}
	z := s.load()
	var client *Client
	for i := range z.Clients {
		if z.Clients[i].From.Contains(from.Unmap()) {
			client = &z.Clients[i]
			break
		}
	}
	if client == nil {
		return reply(h, &q, dnsmessage.RCodeRefused, nil)
	}

	name := strings.ToLower(q.Name.String())
	if name == zone || strings.HasSuffix(name, "."+zone) {
		addr, ok := client.Names[name]
		switch {
		case !ok:
			return reply(h, &q, dnsmessage.RCodeNameError, nil)
		case q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET:
			return reply(h, &q, dnsmessage.RCodeSuccess, &addr)
		default: // the name exists, with no records of that type
			return reply(h, &q, dnsmessage.RCodeSuccess, nil)
		}
	}
	if s.Forward == nil || len(z.Upstreams) == 0 {
		return reply(h, &q, dnsmessage.RCodeRefused, nil)
	}
	key := name + " " + q.Type.String() + " " + q.Class.String()
	cached, fresh := s.cache.get(key, h.ID)
	if fresh {
		return cached
	}
	ans, err := s.Forward(ctx, z.Upstreams, query)
	if err != nil {
		s.logf("%s %s via %v: %v (stale answer: %v)", q.Name, q.Type, z.Upstreams, err, cached != nil)
		if cached != nil {
			return cached
		}
		return reply(h, &q, dnsmessage.RCodeServerFailure, nil)
	}
	s.cache.put(key, ans)
	return ans
}

// reply builds a response to h with q echoed and, if addr is set, one A
// record for it.
func reply(h dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, addr *netip.Addr) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: h.ID, Response: true, OpCode: h.OpCode, RecursionDesired: h.RecursionDesired,
		RecursionAvailable: true, Authoritative: addr != nil || rcode == dnsmessage.RCodeNameError, RCode: rcode,
	})
	b.EnableCompression()
	if q != nil {
		b.StartQuestions()
		b.Question(*q)
	}
	if addr != nil {
		b.StartAnswers()
		b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 5},
			dnsmessage.AResource{A: addr.As4()})
	}
	msg, err := b.Finish()
	if err != nil {
		return nil
	}
	return msg
}

// Serve answers UDP queries on pc and TCP queries on l until ctx ends.
// Either may be nil.
func (s *Server) Serve(ctx context.Context, pc net.PacketConn, l net.Listener) {
	slots := make(chan struct{}, inFlight)
	if pc != nil {
		go func() {
			buf := make([]byte, 65535)
			for {
				n, from, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				src, ok := from.(*net.UDPAddr)
				if !ok {
					continue
				}
				select {
				case slots <- struct{}{}:
				default:
					continue
				}
				query := append([]byte(nil), buf[:n]...)
				go func() {
					defer func() { <-slots }()
					ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					if ans := s.Answer(ctx, src.AddrPort().Addr(), query); ans != nil {
						pc.WriteTo(ans, from)
					}
				}()
			}
		}()
	}
	if l != nil {
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				select {
				case slots <- struct{}{}:
					go func() {
						defer func() { <-slots }()
						s.serveTCP(ctx, c)
					}()
				default:
					c.Close()
				}
			}
		}()
	}
	<-ctx.Done()
	if pc != nil {
		pc.Close()
	}
	if l != nil {
		l.Close()
	}
}

// serveTCP answers length-prefixed queries on c until it goes quiet.
func (s *Server) serveTCP(ctx context.Context, c net.Conn) {
	defer c.Close()
	src, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	for {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		var size [2]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return
		}
		query := make([]byte, int(size[0])<<8|int(size[1]))
		if _, err := io.ReadFull(c, query); err != nil {
			return
		}
		ans := s.Answer(ctx, src.AddrPort().Addr(), query)
		if ans == nil {
			return
		}
		if _, err := c.Write(append([]byte{byte(len(ans) >> 8), byte(len(ans))}, ans...)); err != nil {
			return
		}
	}
}
