// Command cyac-init is PID 1 wherever cyac supplies one:
//
//   - In a Firecracker VM it is the initramfs's /init. It mounts the box's
//     disk, writes its resolv.conf and networkd config, switches root to
//     it, and starts the exec agent ("cyac-init agent", see agent.go).
//     Then it runs the box's init, its command, or nothing (see
//     agent.Config.Mode).
//   - Under runc it is PID 1 of boxes in command or pause mode, so their
//     orphans are reaped and a stop signal stops them.
//
// It must be a static binary: it runs before, and beside, any image.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containmentzone/cyacrt/internal/agent"
)

const (
	self       = "/run/cyac/init"
	configPath = "/run/cyac/config.json"
	defaultEnv = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		fatal(serveAgent())
	}
	if os.Getpid() != 1 {
		fatal(fmt.Errorf("must run as PID 1"))
	}
	_, err := os.Stat("/cyac.json")
	vm := err == nil
	if vm {
		if err := switchRoot(); err != nil {
			powerOff(true, "boot: "+err.Error())
		}
	}
	var cfg agent.Config
	data, err := os.ReadFile(configPath)
	if err == nil {
		err = json.Unmarshal(data, &cfg)
	}
	if err == nil && cfg.Mode != "pause" && len(cfg.Args) == 0 {
		err = fmt.Errorf("no command in %s", configPath)
	}
	if err != nil {
		powerOff(vm, err.Error())
	}
	if vm {
		// Without the agent the box still boots; only exec is missing.
		if err := startAgent(); err != nil {
			logf("agent: %v", err)
		}
	}
	if cfg.Mode == "" {
		err := syscall.Exec(cfg.Args[0], cfg.Args, os.Environ())
		powerOff(vm, fmt.Sprintf("exec %s: %v", cfg.Args[0], err))
	}
	supervise(cfg, vm)
}

// switchRoot mounts the box's disk at /newroot, sets up its API
// filesystems and /run/cyac, and makes it the root.
func switchRoot() error {
	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	if err := syscall.Mount("/dev/vda", "/newroot", "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount /dev/vda: %w", err)
	}
	// systemd and NixOS's stage 2 take these over as they find them.
	for _, m := range []struct{ dir, fstype, opts string }{
		{"dev", "devtmpfs", ""},
		{"dev/pts", "devpts", "mode=0620,ptmxmode=0666"},
		{"proc", "proc", ""},
		{"sys", "sysfs", ""},
		{"run", "tmpfs", "mode=0755"},
	} {
		dir := "/newroot/" + m.dir
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(m.fstype, dir, m.fstype, syscall.MS_NOSUID, m.opts); err != nil {
			return fmt.Errorf("mount %s: %w", dir, err)
		}
	}
	// The agent and command mode run from here once the initramfs is gone.
	if err := os.MkdirAll("/newroot/run/cyac", 0o755); err != nil {
		return err
	}
	if err := copyFile("/init", "/newroot"+self, 0o755); err != nil {
		return err
	}
	if err := copyFile("/cyac.json", "/newroot"+configPath, 0o644); err != nil {
		return err
	}
	data, err := os.ReadFile("/cyac.json")
	if err != nil {
		return err
	}
	var cfg agent.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if err := agent.WriteNetConfig("/newroot", cfg); err != nil {
		return err
	}
	if err := os.Chdir("/newroot"); err != nil {
		return err
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move /newroot: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return err
	}
	return os.Chdir("/")
}

var agentPid int

func startAgent() error {
	p, err := os.StartProcess(self, []string{"cyac-init", "agent"}, &os.ProcAttr{
		Dir: "/", Env: []string{defaultEnv}, Files: []*os.File{nil, os.Stdout, os.Stderr},
	})
	if err == nil {
		agentPid = p.Pid
	}
	return err
}

// supervise is PID 1 in command and pause mode: it runs the command,
// reaps orphans, restarts the agent if it dies, and ends the box when the
// command exits or a stop signal arrives (SIGINT from Firecracker's
// Ctrl-Alt-Del, or runc's SIGRTMIN+3, the signal that halts systemd).
func supervise(cfg agent.Config, vm bool) {
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGINT, syscall.SIGTERM, syscall.Signal(37))
	if vm {
		// Ctrl-Alt-Del then sends SIGINT instead of resetting the VM.
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_CAD_OFF)
		os.MkdirAll("/dev/shm", 0o1777)
		syscall.Mount("shm", "/dev/shm", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=1777")
	}
	var cmd *os.Process
	var output <-chan struct{}
	if cfg.Mode == "command" {
		var err error
		if cmd, output, err = startCommand(cfg); err != nil {
			powerOff(vm, err.Error())
		}
		logf("started %s (pid %d)", strings.Join(cfg.Args, " "), cmd.Pid)
	} else {
		logf("pause: no command; the box stays up for exec")
	}
	var deadline <-chan time.Time
	for {
		select {
		case <-deadline:
			cmd.Kill()
		case sig := <-sigs:
			if sig != syscall.SIGCHLD {
				if cmd == nil {
					powerOff(vm, "stopped")
				}
				logf("stopping: %v", sig)
				cmd.Signal(syscall.SIGTERM)
				deadline = time.After(10 * time.Second)
				continue
			}
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				switch {
				case cmd != nil && pid == cmd.Pid:
					select { // let its last output reach the console
					case <-output:
					case <-time.After(time.Second):
					}
					powerOff(vm, fmt.Sprintf("%s exited (%s)", cfg.Args[0], describe(ws)))
				case vm && pid == agentPid:
					logf("agent exited (%s), restarting it", describe(ws))
					time.Sleep(time.Second)
					startAgent()
				}
			}
		}
	}
}

func describe(ws syscall.WaitStatus) string {
	if ws.Signaled() {
		return "signal " + ws.Signal().String()
	}
	return "status " + strconv.Itoa(ws.ExitStatus())
}

// startCommand runs the box's command with the image's environment,
// working directory and user. Its output goes through a pipe it owns, as
// under Docker, so it can reopen /dev/stdout (nginx's log files are
// symlinks to it); PID 1 copies that to the console, and closes the
// returned channel when the pipe is drained.
func startCommand(cfg agent.Config) (*os.Process, <-chan struct{}, error) {
	env := cfg.Env
	if !hasVar(env, "PATH") {
		env = append(env, defaultEnv)
	}
	cred, home, err := credential(cfg.User)
	if err != nil {
		return nil, nil, err
	}
	if !hasVar(env, "HOME") {
		env = append(env, "HOME="+home)
	}
	path, err := lookPath(cfg.Args[0], env)
	if err != nil {
		return nil, nil, err
	}
	dir := cfg.Cwd
	if dir == "" {
		dir = "/"
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	defer w.Close()
	if cred != nil {
		if err := w.Chown(int(cred.Uid), int(cred.Gid)); err != nil {
			return nil, nil, err
		}
	}
	p, err := os.StartProcess(path, cfg.Args, &os.ProcAttr{
		Dir: dir, Env: env, Files: []*os.File{os.Stdin, w, w},
		Sys: &syscall.SysProcAttr{Setsid: true, Credential: cred},
	})
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		io.Copy(os.Stdout, r)
		close(done)
	}()
	return p, done, nil
}

func hasVar(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// lookPath finds name on the PATH in env, as exec.LookPath would with it.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, kv := range env {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, dir := range filepath.SplitList(p) {
				path := filepath.Join(dir, name)
				if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
					return path, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%s not found on the image's PATH", name)
}

// credential resolves an image USER (user, uid, user:group or uid:gid)
// against the image's /etc/passwd and /etc/group.
func credential(spec string) (*syscall.Credential, string, error) {
	if spec == "" || spec == "root" || spec == "0" {
		return nil, "/root", nil
	}
	user, group, _ := strings.Cut(spec, ":")
	cred := &syscall.Credential{Groups: []uint32{}}
	home := "/"
	if f := lookup("/etc/passwd", user); len(f) >= 7 {
		cred.Uid, cred.Gid, home = atoi(f[2]), atoi(f[3]), f[5]
	} else if uid, err := strconv.ParseUint(user, 10, 32); err == nil {
		cred.Uid, cred.Gid = uint32(uid), uint32(uid)
	} else {
		return nil, "", fmt.Errorf("user %s isn't in the image's /etc/passwd", user)
	}
	if group != "" {
		if f := lookup("/etc/group", group); f != nil {
			cred.Gid = atoi(f[2])
		} else if gid, err := strconv.ParseUint(group, 10, 32); err == nil {
			cred.Gid = uint32(gid)
		} else {
			return nil, "", fmt.Errorf("group %s isn't in the image's /etc/group", group)
		}
	}
	return cred, home, nil
}

// lookup returns the fields of the line in a passwd-style file whose name
// or id is key.
func lookup(file, key string) []string {
	data, _ := os.ReadFile(file)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 4 && (f[0] == key || f[2] == key) {
			return f
		}
	}
	return nil
}

func atoi(s string) uint32 { n, _ := strconv.ParseUint(s, 10, 32); return uint32(n) }

// powerOff ends the box. In a VM it stops everything, syncs, and resets:
// Firecracker exits on a reset (the kernel runs with reboot=k), and the
// console line tells the daemon not to restart the box.
func powerOff(vm bool, reason string) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", agent.PoweringOff, reason)
	if !vm {
		os.Exit(0)
	}
	syscall.Kill(-1, syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	syscall.Kill(-1, syscall.SIGKILL)
	syscall.Sync()
	syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	select {}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cyac-init: "+format+"\n", args...)
}

func fatal(err error) {
	if err != nil {
		logf("%v", err)
		os.Exit(1)
	}
	os.Exit(0)
}
