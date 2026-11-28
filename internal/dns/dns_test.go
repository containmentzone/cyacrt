package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/lease"
)

// server writes a conf with two networks behind gateway out (dev with
// box a and service proxy, lab with box b) and sealed with box c, and
// returns out's resolver.
func server(t *testing.T, fwd Forward) *Server {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"out.cygateway":    "[Gateway]\nKind=direct\nDNS=9.9.9.9\n",
		"dev.cynetwork":    "[Network]\nAttach=service:proxy\nAttach=gateway:out\n",
		"lab.cynetwork":    "[Network]\nAttach=gateway:out\n",
		"sealed.cynetwork": "[Network]\n",
		"proxy.cyservice":  "[Service]\nImage=i\nGateway=out\nPort=8080\n",
		"a.cybox":          "[Box]\nImage=i\nNetwork=dev\n",
		"b.cybox":          "[Box]\nImage=i\nNetwork=lab\n",
		"c.cybox":          "[Box]\nImage=i\nNetwork=sealed\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, problems, err := conf.Load(dir)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	if err := lease.Assign(t.TempDir(), c, func(string, string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	zones := t.TempDir()
	if err := Write(c, zones); err != nil {
		t.Fatal(err)
	}
	return &Server{File: Path(zones, "out"), Forward: fwd}
}

var (
	boxA  = netip.MustParseAddr("10.42.0.4") // networks lease in name order: dev, lab, sealed
	boxB  = netip.MustParseAddr("10.42.1.3")
	boxC  = netip.MustParseAddr("10.42.2.3")
	proxy = netip.MustParseAddr("10.42.255.1") // service end of its link
	other = netip.MustParseAddr("10.42.9.10")  // a network without a gateway
)

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// parse returns the reply's rcode and the A records in it.
func parse(t *testing.T, msg []byte) (dnsmessage.RCode, []netip.Addr) {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(msg); err != nil {
		t.Fatal(err)
	}
	if m.ID != 7 || !m.Response {
		t.Errorf("header %+v", m.Header)
	}
	var addrs []netip.Addr
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			addrs = append(addrs, netip.AddrFrom4(r.A))
		}
	}
	return m.RCode, addrs
}

func TestZone(t *testing.T) {
	var forwarded []string
	s := server(t, func(_ context.Context, upstreams []string, q []byte) ([]byte, error) {
		forwarded = upstreams
		return q, nil // echo, enough to see it went upstream
	})
	for _, tc := range []struct {
		from  netip.Addr
		name  string
		typ   dnsmessage.Type
		rcode dnsmessage.RCode
		addr  string
	}{
		{boxA, "a.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.0.4"},
		{boxA, "A.Cyac.Internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.0.4"},
		{boxA, "proxy.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.0.3"},
		{boxA, "a.cyac.internal.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, ""},
		{boxA, "b.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""}, // on lab
		{boxA, "x.a.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""},
		// lab shares the gateway but sees only its own names
		{boxB, "b.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.3"},
		{boxB, "a.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""},
		// a service sees the networks that attach it
		{proxy, "a.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.0.4"},
		{proxy, "b.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""},
		// anyone not behind out is refused
		{other, "a.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeRefused, ""},
		{boxC, "c.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeRefused, ""},
		{boxC, "example.com.", dnsmessage.TypeA, dnsmessage.RCodeRefused, ""},
	} {
		rcode, addrs := parse(t, s.Answer(context.Background(), tc.from, query(t, tc.name, tc.typ)))
		want := 0
		if tc.addr != "" {
			want = 1
		}
		if rcode != tc.rcode || len(addrs) != want || want == 1 && addrs[0].String() != tc.addr {
			t.Errorf("%v %s %v: %v %v, want %v %s", tc.from, tc.name, tc.typ, rcode, addrs, tc.rcode, tc.addr)
		}
	}
	if forwarded != nil {
		t.Errorf("zone question went upstream")
	}
	q := query(t, "example.com.", dnsmessage.TypeA)
	if ans := s.Answer(context.Background(), proxy, q); string(ans) != string(q) || len(forwarded) != 1 || forwarded[0] != "9.9.9.9" {
		t.Errorf("outside name not forwarded to DNS=: %v", forwarded)
	}
}

func TestNoZoneFile(t *testing.T) {
	s := &Server{File: filepath.Join(t.TempDir(), "none.json"), Forward: func(context.Context, []string, []byte) ([]byte, error) {
		t.Error("forwarded without a zone")
		return nil, nil
	}}
	if rcode, _ := parse(t, s.Answer(context.Background(), boxA, query(t, "example.com.", dnsmessage.TypeA))); rcode != dnsmessage.RCodeRefused {
		t.Errorf("rcode %v", rcode)
	}
}

func TestUpstreamFails(t *testing.T) {
	s := server(t, func(context.Context, []string, []byte) ([]byte, error) { return nil, errors.New("down") })
	if rcode, _ := parse(t, s.Answer(context.Background(), boxA, query(t, "example.com.", dnsmessage.TypeA))); rcode != dnsmessage.RCodeServerFailure {
		t.Errorf("rcode %v", rcode)
	}
	if s.Answer(context.Background(), boxA, []byte{1, 2, 3}) != nil {
		t.Error("answered garbage")
	}
}

// Serve speaks both transports.
func TestServe(t *testing.T) {
	s := server(t, nil)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx, pc, l)

	// The source is 127.0.0.1, which is no client: refused, over both.
	q := query(t, "a.cyac.internal.", dnsmessage.TypeA)
	u, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	u.SetDeadline(time.Now().Add(5 * time.Second))
	u.Write(q)
	buf := make([]byte, 512)
	n, err := u.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if rcode, _ := parse(t, buf[:n]); rcode != dnsmessage.RCodeRefused {
		t.Errorf("udp: %v", rcode)
	}

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write(append([]byte{0, byte(len(q))}, q...))
	if _, err := c.Read(buf[:2]); err != nil {
		t.Fatal(err)
	}
	size := int(buf[0])<<8 | int(buf[1])
	n, err = c.Read(buf[:size])
	if err != nil || n != size {
		t.Fatal(n, err)
	}
	if rcode, _ := parse(t, buf[:n]); rcode != dnsmessage.RCodeRefused {
		t.Errorf("tcp: %v", rcode)
	}
}

// forward asks again when a UDP packet is lost, and over TCP when the
// UDP answer is truncated.
func TestForwardRetries(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	l, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	answer := func(query []byte, truncate bool) []byte {
		var p dnsmessage.Parser
		h, _ := p.Start(query)
		q, _ := p.Question()
		if truncate {
			ans := reply(h, &q, dnsmessage.RCodeSuccess, nil)
			ans[2] |= 0x02
			return ans
		}
		addr := netip.MustParseAddr("192.0.2.1")
		return reply(h, &q, dnsmessage.RCodeSuccess, &addr)
	}
	var udp atomic.Int32
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if udp.Add(1) > 1 { // the first packet is lost
				pc.WriteTo(answer(buf[:n], true), from)
			}
		}
	}()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			var size [2]byte
			c.Read(size[:])
			q := make([]byte, int(size[0])<<8|int(size[1]))
			c.Read(q)
			ans := answer(q, false)
			c.Write(append([]byte{byte(len(ans) >> 8), byte(len(ans))}, ans...))
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans, err := forward(ctx, net.Dial, []string{pc.LocalAddr().String()}, query(t, "example.com.", dnsmessage.TypeA))
	if err != nil {
		t.Fatal(err)
	}
	if _, addrs := parse(t, ans); len(addrs) != 1 || truncated(ans) || udp.Load() != 2 {
		t.Errorf("addrs %v, truncated %v, udp tries %d", addrs, truncated(ans), udp.Load())
	}
}

// Forwarded answers are served from the cache while fresh, and after
// that only when upstream fails.
func TestCache(t *testing.T) {
	calls, fail := 0, false
	s := server(t, func(_ context.Context, _ []string, q []byte) ([]byte, error) {
		calls++
		if fail {
			return nil, errors.New("down")
		}
		var p dnsmessage.Parser
		h, _ := p.Start(q)
		qq, _ := p.Question()
		addr := netip.MustParseAddr("192.0.2.1")
		return reply(h, &qq, dnsmessage.RCodeSuccess, &addr), nil
	})
	ask := func(id uint16) []byte {
		q := query(t, "Example.com.", dnsmessage.TypeA)
		q[0], q[1] = byte(id>>8), byte(id)
		ans := s.Answer(context.Background(), boxA, q)
		if ans[0] != q[0] || ans[1] != q[1] {
			t.Fatalf("answer has ID %x%x, want %x", ans[0], ans[1], id)
		}
		return ans
	}
	ask(1) // cached with ID 1, served below with 7
	if _, addrs := parse(t, ask(7)); len(addrs) != 1 || calls != 1 {
		t.Fatalf("fresh: addrs %v, %d calls", addrs, calls)
	}

	// Expire it: now it is asked for again, and served stale when that fails.
	for k, e := range s.cache.entries {
		e.fresh = time.Now().Add(-time.Second)
		s.cache.entries[k] = e
	}
	fail = true
	if rcode, addrs := parse(t, ask(7)); rcode != dnsmessage.RCodeSuccess || len(addrs) != 1 || calls != 2 {
		t.Fatalf("stale: %v %v, %d calls", rcode, addrs, calls)
	}
	if rcode, _ := parse(t, s.Answer(context.Background(), boxA, query(t, "other.com.", dnsmessage.TypeA))); rcode != dnsmessage.RCodeServerFailure {
		t.Errorf("uncached failure: %v", rcode)
	}
}
