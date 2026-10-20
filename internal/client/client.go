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

// Exec runs args in the box with the local terminal attached.
func (c *Client) Exec(name string, args []string) error {
	req := api.Exec{Args: args}
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		if w, h, err := term.GetSize(fd); err == nil {
			req.Cols, req.Rows = uint16(w), uint16(h)
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hreq, err := http.NewRequest("POST", "http://cyac/v1/boxes/"+name+"/exec", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Connection", "Upgrade")
	hreq.Header.Set("Upgrade", api.ExecUpgrade)

	conn, err := net.Dial("unix", c.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := hreq.Write(conn); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, hreq)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		var e api.Error
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			return errors.New(resp.Status)
		}
		return fmt.Errorf("exec: %s", e.Error)
	}

	if term.IsTerminal(fd) {
		old, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer term.Restore(fd, old)
	}
	go io.Copy(conn, os.Stdin)
	_, err = io.Copy(os.Stdout, br) // ends when the command exits
	return err
}
