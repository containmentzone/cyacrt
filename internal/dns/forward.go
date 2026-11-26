package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

// udpTry is how long one UDP attempt waits for the upstream before
// asking again. Clients give up after 5s, so two tries and a TCP
// fallback still answer before they do.
const udpTry = 2 * time.Second

// Upstream is the Forward the resolver uses: it runs in the gateway's
// netns, so an ordinary dial takes the gateway's route out (its uplink or
// its tunnel) and nothing else.
func Upstream(ctx context.Context, upstreams []string, query []byte) ([]byte, error) {
	var addrs []string
	for _, ip := range upstreams {
		addrs = append(addrs, net.JoinHostPort(ip, "53"))
	}
	dial := func(network, addr string) (net.Conn, error) { return net.DialTimeout(network, addr, 3*time.Second) }
	return forward(ctx, dial, addrs, query)
}

// forward asks the first upstream over UDP, the next one (or the same,
// if there is only one) if the first packet goes unanswered, and the one
// that answered over TCP if its answer was truncated. Without the
// TCP step a client that retries over TCP itself (glibc does, for
// answers over 512 bytes) gets the same truncated, usually empty,
// answer again, which it takes for "no such records".
func forward(ctx context.Context, dial func(network, addr string) (net.Conn, error), upstreams []string, query []byte) ([]byte, error) {
	var ans []byte
	var err error
	var addr string
	for try := range 2 {
		addr = upstreams[try%len(upstreams)]
		if ans, err = exchange(ctx, dial, "udp", addr, query); err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if truncated(ans) {
		return exchange(ctx, dial, "tcp", addr, query)
	}
	return ans, nil
}

// exchange sends one query over a new connection and reads one answer
// with the same ID. A UDP attempt waits at most udpTry.
func exchange(ctx context.Context, dial func(network, addr string) (net.Conn, error), network, addr string, query []byte) ([]byte, error) {
	c, err := dial(network, addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if try := time.Now().Add(udpTry); network == "udp" && try.Before(deadline) {
		deadline = try
	}
	c.SetDeadline(deadline)

	var ans []byte
	if network == "udp" {
		if _, err := c.Write(query); err != nil {
			return nil, err
		}
		buf := make([]byte, 65535)
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		ans = buf[:n]
	} else {
		msg := binary.BigEndian.AppendUint16(nil, uint16(len(query)))
		if _, err := c.Write(append(msg, query...)); err != nil {
			return nil, err
		}
		var size [2]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return nil, err
		}
		ans = make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(c, ans); err != nil {
			return nil, err
		}
	}
	if len(ans) < 12 || len(query) < 2 || ans[0] != query[0] || ans[1] != query[1] {
		return nil, errors.New("upstream answer is short or has the wrong ID")
	}
	return ans, nil
}

// truncated reports whether a DNS message has the TC flag set.
func truncated(msg []byte) bool { return len(msg) > 2 && msg[2]&0x02 != 0 }
