package dns

import (
	"context"
	"net"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/render"
)

// ViaGateway is the Forward that asks the gateway's resolver over UDP
// from a socket opened inside the gateway's netns. The socket keeps that
// netns after the thread leaves it, so the query takes the gateway's
// route out (its uplink or its tunnel) and nothing else.
func ViaGateway(ctx context.Context, gw *conf.Gateway, query []byte) ([]byte, error) {
	upstream := gw.DNS
	if upstream == "" {
		upstream = DefaultUpstream
	}
	c, err := dialIn("/run/netns/"+render.GatewayNetns(gw.Name), net.JoinHostPort(upstream, "53"))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	c.SetDeadline(deadline)
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// dialIn opens a UDP socket to addr inside the netns at path. It runs on
// a thread of its own that never leaves that netns: the goroutine exits
// still locked to it, so Go discards the thread instead of reusing it.
func dialIn(path, addr string) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			done <- result{nil, err}
			return
		}
		err = unix.Setns(fd, unix.CLONE_NEWNET)
		unix.Close(fd)
		if err != nil {
			done <- result{nil, err}
			return
		}
		c, err := net.Dial("udp", addr)
		done <- result{c, err}
	}()
	r := <-done
	return r.c, r.err
}
