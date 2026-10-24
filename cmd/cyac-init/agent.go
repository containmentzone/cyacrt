package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/containmentzone/cyacrt/internal/agent"
)

// serveAgent runs commands for the daemon, which reaches it through
// Firecracker's vsock device (see agent.Exec for the protocol). Commands
// run as root; the daemon wraps them in runuser for the login user.
func serveAgent() error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: agent.Port}); err != nil {
		return fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		return err
	}
	for {
		conn, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			return err
		}
		go session(os.NewFile(uintptr(conn), "vsock"))
	}
}

// session runs one command on a pty: terminal input comes in raw after
// the request line, output and the exit status go back as frames.
func session(conn io.ReadWriteCloser) {
	defer conn.Close()
	var mu sync.Mutex // frames come from two goroutines
	send := func(typ byte, p []byte) {
		mu.Lock()
		defer mu.Unlock()
		agent.WriteFrame(conn, typ, p)
	}
	exit := func(status int) {
		var p [4]byte
		binary.BigEndian.PutUint32(p[:], uint32(int32(status)))
		send(agent.Exit, p[:])
	}

	r := bufio.NewReader(conn)
	var req agent.Exec
	line, err := r.ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	if err == nil && len(req.Args) == 0 {
		err = fmt.Errorf("no command")
	}
	if err != nil {
		send(agent.Output, []byte("cyac-init: "+err.Error()+"\r\n"))
		exit(127)
		return
	}
	cmd := exec.Command(req.Args[0], req.Args[1:]...)
	cmd.Dir = "/"
	// NixOS keeps its tools out of the usual directories.
	cmd.Env = []string{defaultEnv + ":/run/wrappers/bin:/run/current-system/sw/bin", "HOME=/root", "TERM=xterm-256color"}
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
	if err != nil {
		send(agent.Output, []byte("cyac-init: "+err.Error()+"\r\n"))
		exit(127)
		return
	}
	defer tty.Close()
	// Input ends when the host hangs up; closing the pty then hangs up the
	// command, as a dropped ssh session would.
	go func() {
		io.Copy(tty, r)
		tty.Close()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := tty.Read(buf) // EIO once the command and its children let go
		if n > 0 {
			send(agent.Output, buf[:n])
		}
		if err != nil {
			break
		}
	}
	cmd.Wait()
	status := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		status = 128 + int(ws.Signal())
	}
	exit(status)
}
