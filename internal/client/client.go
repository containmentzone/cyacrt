// Package client talks to the cyac daemon over its unix socket.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"golang.org/x/term"

	"github.com/containmentzone/cyacrt/internal/api"
)

type Client struct {
	Socket string
	http   *http.Client
}

func New(socket string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	return &Client{Socket: socket, http: &http.Client{Transport: &http.Transport{DialContext: dial}}}
}

// Do sends in (if non-nil) as JSON and decodes the reply into out (if non-nil).
func (c *Client) Do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://cyac"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e api.Error
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			return errors.New(resp.Status)
		}
		return errors.New(e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Create seeds and starts a box, calling progress for each step the
// daemon reports.
func (c *Client) Create(req api.NewBox, progress func(string)) (*api.Box, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Post("http://cyac/v1/boxes", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e api.Error
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			return nil, errors.New(resp.Status)
		}
		return nil, errors.New(e.Error)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var p api.Progress
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("daemon stopped replying: %w", err)
		}
		switch {
		case p.Error != "":
			return nil, errors.New(p.Error)
		case p.Box != nil:
			return p.Box, nil
		default:
			progress(p.Message)
		}
	}
}

// Exec runs args in the box with the local terminal attached.
func (c *Client) Exec(name string, args []string) error {
	req := api.Exec{Args: args}
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		if w, h, err := term.GetSize(fd); err == nil {
			req.Cols, req.Rows = uint16(w), uint16(h)
		}
	}
	conn, br, err := c.upgrade("/v1/boxes/"+name+"/exec", api.ExecUpgrade, req)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	defer conn.Close()
	defer rawTerminal()()
	go io.Copy(conn, os.Stdin)
	_, err = io.Copy(os.Stdout, br) // ends when the command exits
	return err
}

// DetachKey ends a console session: Ctrl-], as in telnet.
const DetachKey = 0x1d

// Console attaches the local terminal to the box's console until the user
// presses DetachKey.
func (c *Client) Console(name string) error {
	conn, br, err := c.upgrade("/v1/boxes/"+name+"/console", api.ConsoleUpgrade, nil)
	if err != nil {
		return fmt.Errorf("console: %w", err)
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "attached to %s's console; Ctrl-] detaches\n", name)
	defer rawTerminal()()

	detached := make(chan struct{})
	go func() {
		p := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(p)
			if i := bytes.IndexByte(p[:n], DetachKey); i >= 0 {
				conn.Write(p[:i])
				close(detached)
				conn.Close()
				return
			}
			if _, werr := conn.Write(p[:n]); err != nil || werr != nil {
				return
			}
		}
	}()
	_, err = io.Copy(os.Stdout, br)
	select {
	case <-detached:
		return nil
	default:
		return err
	}
}

// rawTerminal puts stdin in raw mode if it is a terminal and returns the
// function that restores it.
func rawTerminal() func() {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}
	}
	return func() { term.Restore(fd, old) }
}

// upgrade POSTs body (if non-nil) to path asking to switch to token, and
// returns the raw connection once the daemon answers 101.
func (c *Client) upgrade(path, token string, body any) (net.Conn, *bufio.Reader, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest("POST", "http://cyac"+path, r)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", token)

	conn, err := net.Dial("unix", c.Socket)
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := func() (*http.Response, error) {
		if err := req.Write(conn); err != nil {
			return nil, err
		}
		return http.ReadResponse(br, req)
	}()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		var e api.Error
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			return nil, nil, errors.New(resp.Status)
		}
		return nil, nil, errors.New(e.Error)
	}
	return conn, br, nil
}
