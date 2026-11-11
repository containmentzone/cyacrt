// Package dns is the resolver on a network's .2: it answers for the
// network's zone, <name>.<network>.cyac.internal, from the conf (its
// boxes and the services it attaches), and forwards every other question
// to its gateway's resolver from inside the gateway's netns, so a box can
// only resolve outside names along the path its packets could take. A
// sealed network gets no forwarding at all.
package dns

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/render"
)

// DefaultUpstream is the resolver for a gateway without DNS=.
const DefaultUpstream = "1.1.1.1"

// Forward sends a raw query upstream through gateway gw and returns the
// raw reply.
type Forward func(ctx context.Context, gw *conf.Gateway, query []byte) ([]byte, error)

// Server answers for one network. It rereads the conf at most once a
// second, so it follows edits without a restart.
type Server struct {
	Conf    string // conf dir
	Network string
	Forward Forward

	mu     sync.Mutex
	loaded time.Time
	zone   string                // "<network>.cyac.internal."
	names  map[string]netip.Addr // fully qualified, lower case
	gw     *conf.Gateway         // nil: sealed
}

// load refreshes the zone from the conf if it is more than a second old.
// A network that is missing or broken has an empty zone and no gateway.
func (s *Server) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.loaded) < time.Second {
		return
	}
	s.loaded = time.Now()
	s.names, s.gw = map[string]netip.Addr{}, nil
	c, _, err := conf.Load(s.Conf)
	n := c.Networks[s.Network]
	if err != nil || n == nil {
		s.zone = s.Network + ".cyac.internal."
		return
	}
	s.zone = render.Domain(n) + "."
	for _, name := range conf.Sorted(c.Boxes) {
		if b := c.Boxes[name]; b.Network == n.Name {
			s.names[name+"."+s.zone] = b.Address
		}
	}
	for _, name := range n.Services {
		s.names[name+"."+s.zone] = c.Services[name].AddressIn(n)
	}
	if n.Gateway != "" {
		s.gw = c.Gateways[n.Gateway]
	}
}

// Answer returns the reply to one query, or nil to drop it.
func (s *Server) Answer(ctx context.Context, query []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return reply(h, nil, dnsmessage.RCodeFormatError, nil)
	}
	s.load()
	s.mu.Lock()
	zone, names, gw := s.zone, s.names, s.gw
	s.mu.Unlock()

	name := strings.ToLower(q.Name.String())
	if name == zone || strings.HasSuffix(name, "."+zone) {
		addr, ok := names[name]
		switch {
		case !ok:
			return reply(h, &q, dnsmessage.RCodeNameError, nil)
		case q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET:
			return reply(h, &q, dnsmessage.RCodeSuccess, &addr)
		default: // the name exists, with no records of that type
			return reply(h, &q, dnsmessage.RCodeSuccess, nil)
		}
	}
	if gw == nil || s.Forward == nil {
		return reply(h, &q, dnsmessage.RCodeRefused, nil)
	}
	ans, err := s.Forward(ctx, gw, query)
	if err != nil {
		return reply(h, &q, dnsmessage.RCodeServerFailure, nil)
	}
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
	if pc != nil {
		go func() {
			buf := make([]byte, 65535)
			for {
				n, from, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				query := append([]byte(nil), buf[:n]...)
				go func() {
					ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					if ans := s.Answer(ctx, query); ans != nil {
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
				go s.serveTCP(ctx, c)
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
		ans := s.Answer(ctx, query)
		if ans == nil {
			return
		}
		if _, err := c.Write(append([]byte{byte(len(ans) >> 8), byte(len(ans))}, ans...)); err != nil {
			return
		}
	}
}
