package box

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/sh"
)

// machine is the driver-facing view of a box.
func (s Store) machine(b *Box) *driver.Machine {
	key := s.SSHKey(b.Name)
	return &driver.Machine{
		Name: b.Name, ID: b.ID, Disk: s.DiskPath(b.Name), RunDir: s.RunDir(b.Name),
		ConsoleLog: s.ConsoleLog(b.Name), ConsoleIn: s.ConsoleIn(b.Name),
		SSHKey: key, KnownHosts: key + ".known_hosts",
		Init: b.Init, CPUs: b.CPUs, MemMiB: b.MemMiB, Addr: s.Net.Addr(b.ID),
	}
}

// pickRuntime returns the first available runtime, or the one named by
// CYAC_RUNTIME, and logs why it skipped the others.
func (s Store) pickRuntime() (driver.Runtime, error) {
	want := os.Getenv("CYAC_RUNTIME")
	var reasons []string
	for _, rt := range s.Runtimes {
		if want != "" && rt.Name() != want {
			continue
		}
		err := rt.Available()
		if err == nil {
			if len(reasons) > 0 {
				slog.Info("skipped runtimes", "reasons", strings.Join(reasons, "; "))
			}
			return rt, nil
		}
		reasons = append(reasons, fmt.Sprintf("%s: %v", rt.Name(), err))
	}
	return nil, fmt.Errorf("no usable runtime (%s)", strings.Join(reasons, "; "))
}

// running returns the runtime a box is running under, or nil if stopped.
// The runtime's name is recorded in the run dir at start.
func (s Store) running(b *Box) driver.Runtime {
	name, err := os.ReadFile(filepath.Join(s.RunDir(b.Name), "runtime"))
	if err != nil {
		return nil
	}
	for _, rt := range s.Runtimes {
		if rt.Name() == string(name) && rt.Running(s.machine(b)) {
			return rt
		}
	}
	return nil
}

// Status is "stopped" or "running (<runtime>)".
func (s Store) Status(b *Box) string {
	if rt := s.running(b); rt != nil {
		return "running (" + rt.Name() + ")"
	}
	return "stopped"
}

func (s Store) Start(b *Box) error {
	if rt := s.running(b); rt != nil {
		return fmt.Errorf("%s is already running (%s)", b.Name, rt.Name())
	}
	s.cleanup(b) // leftovers from a crash or host reboot
	rt, err := s.pickRuntime()
	if err != nil {
		return err
	}
	if err := s.Net.Setup(); err != nil {
		return err
	}
	m := s.machine(b)
	slog.Info("starting box", "box", b.Name, "runtime", rt.Name(), "ip", m.Addr.IP, "cpus", b.CPUs, "mem_mib", b.MemMiB)
	if err := os.MkdirAll(m.RunDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.RunDir, "runtime"), []byte(rt.Name()), 0o600); err != nil {
		return err
	}
	if err := syscall.Mkfifo(m.ConsoleIn, 0o600); err != nil {
		return err
	}
	logStart := rotate(m.ConsoleLog)
	err = s.Net.Attach(m, rt.LinkKind())
	if err == nil {
		err = rt.Start(m)
	}
	if err != nil {
		console := tail(m.ConsoleLog, logStart, 2048)
		s.cleanup(b)
		return fmt.Errorf("start %s (%s): %w\n%s", b.Name, rt.Name(), err, console)
	}
	slog.Info("started box", "box", b.Name, "runtime", rt.Name())
	return nil
}

func (s Store) Stop(b *Box) error {
	rt := s.running(b)
	if rt == nil {
		return fmt.Errorf("%s is not running", b.Name)
	}
	slog.Info("stopping box", "box", b.Name, "runtime", rt.Name())
	err := rt.Stop(s.machine(b))
	s.cleanup(b)
	if err == nil {
		slog.Info("stopped box", "box", b.Name)
	}
	return err
}

// rotate keeps one old console log once the current one passes 8 MiB,
// and returns where this run's output starts.
func rotate(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if fi.Size() > 8<<20 && os.Rename(path, path+".1") == nil {
		return 0
	}
	return fi.Size()
}

// tail returns up to n bytes from the end of path, starting no earlier than from.
func tail(path string, from, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	from = min(max(from, fi.Size()-n), fi.Size())
	buf := make([]byte, fi.Size()-from)
	f.ReadAt(buf, from)
	return buf
}

// cleanup removes everything a run leaves behind. Safe to call when stopped.
func (s Store) cleanup(b *Box) {
	m := s.machine(b)
	for _, rt := range s.Runtimes {
		rt.Cleanup(m)
	}
	s.Net.Detach(m)
	os.RemoveAll(m.RunDir)
}

// Resize grows a box's disk. Shrinking is not supported.
func (s Store) Resize(b *Box, sizeBytes int64) error {
	disk := s.DiskPath(b.Name)
	fi, err := os.Stat(disk)
	if err != nil {
		return err
	}
	if sizeBytes <= fi.Size() {
		return errors.New("new size must be larger than the current size")
	}
	slog.Info("resizing box", "box", b.Name, "from_bytes", fi.Size(), "to_bytes", sizeBytes)
	if err := os.Truncate(disk, sizeBytes); err != nil {
		return err
	}
	if rt := s.running(b); rt != nil {
		return rt.Grow(s.machine(b))
	}
	if err := sh.Run("e2fsck", "-f", "-p", disk); err != nil {
		return err
	}
	return sh.Run("resize2fs", disk)
}

// Remove deletes a stopped box and its disk.
func (s Store) Remove(b *Box) error {
	if rt := s.running(b); rt != nil {
		return fmt.Errorf("%s is running; stop it first", b.Name)
	}
	s.cleanup(b)
	if err := os.Remove(s.DiskPath(b.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, f := range []string{s.ConsoleLog(b.Name), s.ConsoleLog(b.Name) + ".1", s.SSHKey(b.Name), s.SSHKey(b.Name) + ".known_hosts"} {
		os.Remove(f)
	}
	if err := s.Delete(b.Name); err != nil {
		return err
	}
	slog.Info("removed box", "box", b.Name)
	return nil
}

// Command returns a command that runs args inside a running box; start it
// with a terminal on its stdio. It runs as the box's login user, if the
// image named one, unless root is set.
func (s Store) Command(b *Box, args []string, root bool) (*exec.Cmd, error) {
	rt := s.running(b)
	if rt == nil {
		return nil, fmt.Errorf("%s is not running", b.Name)
	}
	if b.User != "" && !root {
		// A login shell, so the user's HOME, profile and MOTD apply.
		args = []string{"runuser", "-l", b.User, "-c", sh.Quote(args)}
	}
	return rt.Command(s.machine(b), args)
}

// IP is the box's address, for the proxy and for display.
func (s Store) IP(b *Box) netip.Addr { return s.Net.Addr(b.ID).IP }
