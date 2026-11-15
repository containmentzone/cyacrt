package dns

import (
	"context"
	"encoding/json"
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

// server writes the zone files for a small conf and returns gateway out's
// resolver.
func server(t *testing.T, fwd Forward) *Server {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"out.cygateway":   "[Gateway]\nIndex=0\nKind=direct\nDNS=9.9.9.9\n",
		"dev.cynetwork":   "[Network]\nIndex=1\nAttach=service:proxy\nAttach=gateway:out\n",
		"web.cynetwork":   "[Network]\nIndex=3\nAttach=service:proxy\nAttach=gateway:out\n",
		"lab.cynetwork":   "[Network]\nIndex=2\n",
		"proxy.cyservice": "[Workload]\nImage=i\n\n[Service]\nIndex=0\nGateway=out\nPort=8080\n",
		"a.cybox":         "[Workload]\nImage=i\n\n[Box]\nNetwork=dev\nAddress=10.42.1.10\n",
		"b.cybox":         "[Workload]\nImage=i\n\n[Box]\nNetwork=lab\nAddress=10.42.2.10\n",
		"c.cybox":         "[Workload]\nImage=i\n\n[Box]\nNetwork=web\nAddress=10.42.3.10\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, probs, err := conf.Load(dir)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	zones := t.TempDir()
	if err := Write(c, zones); err != nil {
		t.Fatal(err)
	}
	return &Server{File: Path(zones, "out"), Forward: fwd}
}

var (
	boxA  = netip.MustParseAddr("10.42.1.10")  // on dev
	boxC  = netip.MustParseAddr("10.42.3.10")  // on web
	proxy = netip.MustParseAddr("10.42.255.1") // the service's end of its link
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
	var forwarded string
	s := server(t, func(_ context.Context, upstream string, q []byte) ([]byte, error) {
		forwarded = upstream
		return q, nil // echo, enough to see it went upstream
	})
	for _, tc := range []struct {
		from  netip.Addr
		name  string
		typ   dnsmessage.Type
		rcode dnsmessage.RCode
		addr  string
	}{
		{boxA, "a.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.10"},
		{boxA, "A.Dev.Cyac.Internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.10"},
		{boxA, "proxy.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.254"},
		{boxA, "a.dev.cyac.internal.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, ""},
		{boxA, "b.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""}, // on lab
		{boxA, "x.a.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""},
		// Another network's names don't exist for a box, and aren't forwarded.
		{boxA, "c.web.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, ""},
		{boxC, "c.web.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.3.10"},
		// A service sees every network that attaches it.
		{proxy, "a.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.1.10"},
		{proxy, "c.web.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, "10.42.3.10"},
		// Not behind this gateway: a sealed network's box, the host.
		{netip.MustParseAddr("10.42.2.10"), "a.dev.cyac.internal.", dnsmessage.TypeA, dnsmessage.RCodeRefused, ""},
		{netip.MustParseAddr("10.42.254.0"), "example.com.", dnsmessage.TypeA, dnsmessage.RCodeRefused, ""},
	} {
		rcode, addrs := parse(t, s.Answer(context.Background(), tc.from, query(t, tc.name, tc.typ)))
		want := 0
		if tc.addr != "" {
			want = 1
		}
		if rcode != tc.rcode || len(addrs) != want || want == 1 && addrs[0].String() != tc.addr {
			t.Errorf("%s asks %s %v: %v %v, want %v %s", tc.from, tc.name, tc.typ, rcode, addrs, tc.rcode, tc.addr)
		}
	}
	if forwarded != "" {
		t.Errorf("zone question went upstream")
	}
	q := query(t, "example.com.", dnsmessage.TypeA)
	if ans := s.Answer(context.Background(), boxA, q); string(ans) != string(q) || forwarded != "9.9.9.9" {
		t.Errorf("outside name not forwarded to the gateway's resolver: %q", forwarded)
	}
}

func TestUpstreamFails(t *testing.T) {
	s := server(t, func(context.Context, string, []byte) ([]byte, error) { return nil, errors.New("down") })
	if rcode, _ := parse(t, s.Answer(context.Background(), boxA, query(t, "example.com.", dnsmessage.TypeA))); rcode != dnsmessage.RCodeServerFailure {
		t.Errorf("rcode %v", rcode)
	}
	if s.Answer(context.Background(), boxA, []byte{1, 2, 3}) != nil {
		t.Error("answered garbage")
	}
}

// Without its zone file a resolver refuses everyone.
func TestNoZone(t *testing.T) {
	s := &Server{File: filepath.Join(t.TempDir(), "none.json")}
	if rcode, _ := parse(t, s.Answer(context.Background(), boxA, query(t, "a.dev.cyac.internal.", dnsmessage.TypeA))); rcode != dnsmessage.RCodeRefused {
		t.Errorf("rcode %v", rcode)
	}
}

// Serve speaks both transports.
func TestServe(t *testing.T) {
	// The test's client is on loopback, so that is who the zone admits.
	file := filepath.Join(t.TempDir(), "zone.json")
	zone, _ := json.Marshal(Zone{Clients: []Client{{
		From:  netip.MustParsePrefix("127.0.0.0/8"),
		Names: map[string]netip.Addr{"a.dev.cyac.internal.": boxA},
	}}})
	if err := os.WriteFile(file, zone, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{File: file}
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
