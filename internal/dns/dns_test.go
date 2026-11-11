package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
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
		"a.cybox":         "[Workload]\nImage=i\n\n[Box]\nNetwork=dev\nAddress=10.42.1.10\n",
		"b.cybox":         "[Workload]\nImage=i\n\n[Box]\nNetwork=lab\nAddress=10.42.2.10\n",
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
		{"a.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.10"},
		{"A.Dev.Cyac.Internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.10"},
		{"proxy.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.254"},
		{"a.dev.cyac.internal.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, ""},
		{"b.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""}, // on lab
		{"x.a.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""},
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
	if _, addrs := parse(t, s.Answer(context.Background(), query(t, "b.lab.cyac.internal.", dnsmessage.TypeA))); len(addrs) != 1 {
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
