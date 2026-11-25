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
)

func server(t *testing.T, network string, fwd Forward) *Server {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"out.cygateway":   "[Gateway]\nIndex=0\nKind=direct\nDNS=9.9.9.9\n",
		"dev.cynetwork":   "[Network]\nIndex=1\nAttach=service:proxy\nAttach=gateway:out\n",
		"lab.cynetwork":   "[Network]\nIndex=2\n",
		"proxy.cyservice": "[Workload]\nImage=i\n\n[Service]\nIndex=0\nGateway=out\nPort=8080\n",
		"a.cybox":         "[Box]\nSeedImage=i\nNetwork=dev\nAddress=10.42.1.10\n",
		"b.cybox":         "[Box]\nSeedImage=i\nNetwork=lab\nAddress=10.42.2.10\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{Conf: dir, Network: network, Forward: fwd}
}

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
	var forwarded *conf.Gateway
	s := server(t, "dev", func(_ context.Context, gw *conf.Gateway, q []byte) ([]byte, error) {
		forwarded = gw
		return q, nil // echo, enough to see it went upstream
	})
	for _, tc := range []struct {
		name  string
		typ   dnsmessage.Type
		rcode dnsmessage.RCode
		addr  string
	}{
		{"a.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.10"},
		{"A.Cyac.Internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.10"},
		{"proxy.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.254"},
		{"a.cyac.internal.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, ""},
		{"b.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""}, // on lab
		{"x.a.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""},
		// the old per-network names still answer, but only for this network
		{"a.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.10"},
		{"proxy.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.254"},
		{"b.lab.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""}, // not forwarded either
	} {
		rcode, addrs := parse(t, s.Answer(context.Background(), query(t, tc.name, tc.typ)))
		want := 0
		if tc.addr != "" {
			want = 1
		}
		if rcode != tc.rcode || len(addrs) != want || want == 1 && addrs[0].String() != tc.addr {
			t.Errorf("%s %v: %v %v, want %v %s", tc.name, tc.typ, rcode, addrs, tc.rcode, tc.addr)
		}
	}
	if forwarded != nil {
		t.Errorf("zone question went upstream")
	}
	q := query(t, "example.com.", dnsmessage.TypeA)
	if ans := s.Answer(context.Background(), q); string(ans) != string(q) || forwarded == nil || forwarded.Name != "out" {
		t.Errorf("outside name not forwarded through out: %v", forwarded)
	}
}

func TestSealed(t *testing.T) {
	s := server(t, "lab", func(context.Context, *conf.Gateway, []byte) ([]byte, error) {
		t.Error("a sealed network forwarded")
		return nil, nil
	})
	if rcode, _ := parse(t, s.Answer(context.Background(), query(t, "example.com.", dnsmessage.TypeA))); rcode != dnsmessage.RCodeRefused {
		t.Errorf("rcode %v", rcode)
	}
	if _, addrs := parse(t, s.Answer(context.Background(), query(t, "b.cyac.internal.", dnsmessage.TypeA))); len(addrs) != 1 {
		t.Errorf("own zone: %v", addrs)
	}
}

func TestUpstreamFails(t *testing.T) {
	s := server(t, "dev", func(context.Context, *conf.Gateway, []byte) ([]byte, error) { return nil, errors.New("down") })
	if rcode, _ := parse(t, s.Answer(context.Background(), query(t, "example.com.", dnsmessage.TypeA))); rcode != dnsmessage.RCodeServerFailure {
		t.Errorf("rcode %v", rcode)
	}
	if s.Answer(context.Background(), []byte{1, 2, 3}) != nil {
		t.Error("answered garbage")
	}
}

// Serve speaks both transports.
func TestServe(t *testing.T) {
	s := server(t, "dev", nil)
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

	q := query(t, "a.dev.cyac.internal.", dnsmessage.TypeA)
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
	if _, addrs := parse(t, buf[:n]); len(addrs) != 1 {
		t.Errorf("udp: %v", addrs)
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
	if _, addrs := parse(t, buf[:n]); len(addrs) != 1 {
		t.Errorf("tcp: %v", addrs)
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
	s := server(t, "dev", func(_ context.Context, _ *conf.Gateway, q []byte) ([]byte, error) {
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
		ans := s.Answer(context.Background(), q)
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
	if rcode, _ := parse(t, s.Answer(context.Background(), query(t, "other.com.", dnsmessage.TypeA))); rcode != dnsmessage.RCodeServerFailure {
		t.Errorf("uncached failure: %v", rcode)
	}
}
