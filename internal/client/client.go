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
func (c *Client) Exec(name string, args []string, root bool) error {
	req := api.Exec{Args: args, Root: root}
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

// DetachKey ends a console session: Ctrl-], as in telnet. So does ~. at
// the start of a line, as in ssh, for keyboards where Ctrl-] is awkward.
// Ctrl-C and Ctrl-D go to the box.
const DetachKey = 0x1d

// Console attaches the local terminal to the box's console until the user
// detaches.
func (c *Client) Console(name string) error {
	conn, br, err := c.upgrade("/v1/boxes/"+name+"/console", api.ConsoleUpgrade, nil)
	if err != nil {
		return fmt.Errorf("console: %w", err)
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "attached to %s's console; Ctrl-] or Enter ~ . detaches\n", name)
	defer rawTerminal()()

	detached := make(chan struct{})
	go func() {
		p := make([]byte, 1024)
		esc := escaper{lineStart: true}
		for {
			n, err := os.Stdin.Read(p)
			out, detach := esc.filter(p[:n])
			if _, werr := conn.Write(out); detach {
				close(detached)
				conn.Close()
				return
			} else if err != nil || werr != nil {
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

// escaper finds the detach sequences in console input.
type escaper struct {
	lineStart bool // the last byte ended a line
	tilde     bool // holding a ~ typed at the start of a line
}

// filter returns the bytes to send and whether the user asked to detach.
// After a held ~, . detaches, ~ sends one ~, and anything else sends both.
func (e *escaper) filter(in []byte) (out []byte, detach bool) {
	for _, b := range in {
		switch {
		case b == DetachKey:
			return out, true
		case e.tilde:
			e.tilde = false
			if b == '.' {
				return out, true
			}
			if b != '~' {
				out = append(out, '~')
			}
		case e.lineStart && b == '~':
			e.tilde = true
			continue
		}
		out = append(out, b)
		e.lineStart = b == '\r' || b == '\n'
	}
	return out, false
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
