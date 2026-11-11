// Package agent is what the daemon shares with cyac-init, the PID 1 cyac
// gives VMs and init-less boxes: the box's init config, the initramfs that
// carries both into a VM, and the exec protocol over vsock.
package agent

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// Config tells cyac-init what to run.
type Config struct {
	// Mode is "" to hand PID 1 to Args (the image's init, e.g. systemd),
	// "command" to run Args as a child, like a container, and power off
	// when it exits, or "pause" to run nothing and only serve exec.
	Mode string   `json:"mode,omitempty"`
	Args []string `json:"args,omitempty"`
	// The image's environment, working directory and USER, for commands.
	Env  []string `json:"env,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
	User string   `json:"user,omitempty"`
	// DNS and Network (a systemd-networkd config for eth0) are written
	// into the box on every start, see WriteNetConfig.
	DNS     string `json:"dns,omitempty"`
	Network string `json:"network,omitempty"`
}

// WriteNetConfig writes c's resolver to /etc/resolv.conf and c.Network to
// /etc/systemd/network/10-cyac.network in the root filesystem at dir,
// before the box's init runs. Doing it on every start keeps a box in step
// with the host when cyac's subnet changes. resolv.conf replaces whatever
// the image has there, including a symlink to systemd-resolved's stub,
// which isn't running yet (or at all) when the box needs DNS. dir is
// untrusted image content, so the writes go through os.Root.
func WriteNetConfig(dir string, c Config) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll("etc/systemd/network", 0o755); err != nil {
		return err
	}
	if err := replaceFile(root, "etc/resolv.conf", "# written by cyac on every start\nnameserver "+c.DNS+"\n"); err != nil {
		return err
	}
	return replaceFile(root, "etc/systemd/network/10-cyac.network", c.Network)
}

// replaceFile replaces name in root, without following a symlink there.
func replaceFile(root *os.Root, name, content string) error {
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// InitPath is where cyac-init copies itself in a VM; the agent runs from
// there once the initramfs is gone.
const InitPath = "/run/cyac/init"

// PowerOffSignal is what "cyac-init poweroff" sends PID 1: SIGRTMIN+4,
// which makes systemd power off, and cyac-init stop the box.
const PowerOffSignal = syscall.Signal(38)

// Port is the vsock port the agent listens on in a VM.
const Port = 52

// Exec starts an exec stream, as one line of JSON. After it the host
// sends raw terminal input and the guest sends frames: a type byte, a
// big-endian uint32 length, and that many bytes.
type Exec struct {
	Args []string `json:"args"`
	Rows uint16   `json:"rows"`
	Cols uint16   `json:"cols"`
}

// Frame types, guest to host.
const (
	Output = 'o' // pty output
	Exit   = 'x' // exit status as a big-endian int32; the last frame
)

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, typ byte, p []byte) error {
	hdr := [5]byte{typ}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (typ byte, p []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("frame of %d bytes", n)
	}
	p = make([]byte, n)
	_, err = io.ReadFull(r, p)
	return hdr[0], p, err
}

// Dial connects to the agent through the VMM's vsock socket, which speaks
// Firecracker's CONNECT protocol (Cloud Hypervisor's does too).
func Dial(uds string) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", uds, 5*time.Second)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "CONNECT %d\n", Port)
	// The VMM answers "OK <port>\n", or hangs up if nothing listens.
	// Read a byte at a time so nothing after the line is consumed.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := conn.Read(b); err != nil || b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !strings.HasPrefix(string(line), "OK ") {
		conn.Close()
		return nil, errors.New("the box's agent isn't answering (still booting, or a guest kernel without vsock?)")
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// Run runs args in the VM behind uds with this process's terminal, and
// returns the command's exit status. The daemon runs it on a pty, as
// "cyac agent-exec", the way it would run ssh.
func Run(uds string, args []string) (int, error) {
	conn, err := Dial(uds)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	req := Exec{Args: args}
	fd := int(os.Stdin.Fd())
	if w, h, err := term.GetSize(fd); err == nil {
		req.Cols, req.Rows = uint16(w), uint16(h)
	}
	// The guest's pty does the line discipline; this one passes bytes.
	if st, err := term.MakeRaw(fd); err == nil {
		defer term.Restore(fd, st)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return 0, err
	}
	go io.Copy(conn, os.Stdin)
	r := bufio.NewReader(conn)
	for {
		typ, p, err := ReadFrame(r)
		if err != nil {
			return 0, fmt.Errorf("agent: %w", err)
		}
		switch typ {
		case Output:
			os.Stdout.Write(p)
		case Exit:
			if len(p) != 4 {
				return 0, errors.New("agent: bad exit frame")
			}
			return int(int32(binary.BigEndian.Uint32(p))), nil
		}
	}
}

// PowerOff asks the VM behind uds to power off, by running "cyac-init
// poweroff" there, and returns once that has signalled PID 1.
func PowerOff(uds string) error {
	conn, err := Dial(uds)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(Exec{Args: []string{InitPath, "poweroff"}}); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	for {
		typ, _, err := ReadFrame(r)
		if err != nil || typ == Exit {
			return err
		}
	}
}
