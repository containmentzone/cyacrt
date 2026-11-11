// Command cyac is both the daemon (`cyac daemon`), which owns the state
// directory and serves the proxy, and the CLI, which talks to the daemon
// over a unix socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/containmentzone/cyacrt/internal/agent"
	"github.com/containmentzone/cyacrt/internal/api"
	"github.com/containmentzone/cyacrt/internal/client"
	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/dns"
	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/proxy"
	"github.com/containmentzone/cyacrt/internal/run"
	"github.com/containmentzone/cyacrt/internal/runtime/cloudhypervisor"
	"github.com/containmentzone/cyacrt/internal/runtime/runc"
	"github.com/containmentzone/cyacrt/internal/server"
)

const usage = `usage: cyac <command> [args]

  ls
  start <name>                  systemctl start cybox@<name>
  stop <name>
  shell [--root] <name> [cmd...] (default: bash -l, as the image's login user)
  console <name>                attach to the console; Ctrl-] or Enter ~ . detaches
  expose <name> <port> [--public]  serve the port at https://<name>.<root domain>
  unexpose <name>

  dns <network>                 run by cydns@<network>.socket: the network's resolver
  run start|stop <kind> <name>  run by the units cyac-generator writes:
                                start or stop one object (a box or service
                                runs in the foreground)

  daemon --domain <root domain> [--acme-email <e>] [--group <g>]
                                the proxy serves :80 and :443 with Let's Encrypt;
                                --group lets that group use the socket, like docker's

Boxes, networks, gateways and services are files in /var/lib/cyac/conf
(see cyac-generator); write them by hand for now, then systemctl daemon-reload.

The CLI talks to the daemon on $CYAC_SOCKET (default /run/cyac/cyac.sock).
The daemon keeps state in $CYAC_DIR (default /var/lib/cyac).
CYAC_RUNTIME=cloud-hypervisor, cloud-hypervisor-pvm or runc forces a runtime.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	socket := env("CYAC_SOCKET", api.DefaultSocket)
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "daemon":
		err = daemon(socket, args)
	case "agent-exec":
		// Internal: the daemon runs this on a pty to exec into a VM.
		if len(args) < 2 {
			log.Fatal("usage: cyac agent-exec <vsock socket> <cmd...>")
		}
		var status int
		if status, err = agent.Run(args[0], args[1:]); err == nil {
			os.Exit(status)
		}
	case "run":
		err = runUnit(args)
	case "dns":
		err = serveDNS(args)
	default:
		err = dispatch(client.New(socket), cmd, args)
	}
	if err != nil {
		log.Fatal("cyac: ", err)
	}
}

// runUnit is ExecStart/ExecStop of a generated unit.
func runUnit(args []string) error {
	if len(args) != 3 || (args[0] != "start" && args[0] != "stop") {
		return errors.New("usage: cyac run start|stop <kind> <name>")
	}
	dir := env("CYAC_DIR", "/var/lib/cyac")
	rts, err := runtimes(dir)
	if err != nil {
		return err
	}
	r := &run.Runner{Conf: conf.Dir, Dir: run.Dir, Exec: run.Command(os.Stderr), State: dir, Runtimes: rts}
	if args[0] == "start" {
		// systemd stops a box or service with SIGTERM; Start then stops
		// the workload cleanly and returns.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		return r.Start(ctx, args[1], args[2])
	}
	return r.Stop(args[1], args[2])
}

// serveDNS is cydns@<network>.service. systemd passes the sockets
// (LISTEN_FDS): the datagram one for UDP, the stream one for TCP.
func serveDNS(args []string) error {
	if len(args) != 1 || !conf.ValidName(args[0]) {
		return errors.New("usage: cyac dns <network>")
	}
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		return errors.New("no sockets from systemd; run me from cydns@" + args[0] + ".socket")
	}
	nfds, _ := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	var pc net.PacketConn
	var l net.Listener
	for fd := 3; fd < 3+nfds; fd++ {
		f := os.NewFile(uintptr(fd), "listen")
		if c, err := net.FilePacketConn(f); err == nil {
			pc = c
		} else if ln, err := net.FileListener(f); err == nil {
			l = ln
		}
		f.Close()
	}
	if pc == nil && l == nil {
		return errors.New("systemd passed no usable sockets")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	s := &dns.Server{Conf: conf.Dir, Network: args[0], Forward: dns.ViaGateway}
	s.Serve(ctx, pc, l)
	return nil
}

// runtimes are the runtimes a box can use, in order of preference.
func runtimes(dir string) ([]driver.Runtime, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// The static PID 1 for VMs and init-less boxes; the Nix package sets it.
	init := env("CYAC_INIT", filepath.Join(filepath.Dir(self), "cyac-init"))
	return []driver.Runtime{
		cloudhypervisor.Runtime{Kernel: filepath.Join(dir, "vmlinux"), Init: init},
		cloudhypervisor.Runtime{Kernel: filepath.Join(dir, "vmlinux-pvm"), Init: init, PVM: true},
		runc.Runtime{Init: init},
	}, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dispatch(c *client.Client, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "start", "stop":
		return c.Do("POST", "/v1/boxes/"+parse(fs, args, 1)[0]+"/"+cmd, nil, nil)

	case "ls":
		parse(fs, args, 0)
		var boxes []api.Box
		if err := c.Do("GET", "/v1/boxes", nil, &boxes); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tNETWORK\tIP\tSTATUS\tIMAGE\tROUTE")
		for _, b := range boxes {
			route := ""
			if r := b.Route; r != nil {
				vis := "token"
				if r.Public {
					vis = "public"
				}
				route = fmt.Sprintf("%s→:%d (%s)", r.Host, r.Port, vis)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", b.Name, b.Network, b.IP, b.Status, b.Image, route)
		}
		return w.Flush()

	case "shell":
		root := fs.Bool("root", false, "run as root instead of the image's login user")
		fs.Parse(args) // flags go before the name; the command is passed through as is
		if fs.NArg() < 1 {
			return errors.New("usage: cyac shell [--root] <name> [cmd...]")
		}
		command := fs.Args()[1:]
		if len(command) == 0 {
			command = []string{"/bin/bash", "-l"}
		}
		return c.Exec(fs.Arg(0), command, *root)

	case "console":
		return c.Console(parse(fs, args, 1)[0])

	case "expose":
		public := fs.Bool("public", false, "no token required")
		pos := parse(fs, args, 2)
		port, err := strconv.Atoi(pos[1])
		if err != nil {
			return fmt.Errorf("bad port %q", pos[1])
		}
		var e api.Exposed
		if err := c.Do("PUT", "/v1/boxes/"+pos[0]+"/route", api.Expose{Port: port, Public: *public}, &e); err != nil {
			return err
		}
		fmt.Printf("https://%s\n", e.Host)
		if e.Token != "" {
			fmt.Printf("token: %s\nbrowser: https://%s/__cyac/login?token=%s\n", e.Token, e.Host, e.Token)
		}
		return nil

	case "unexpose":
		return c.Do("DELETE", "/v1/boxes/"+parse(fs, args, 1)[0]+"/route", nil, nil)

	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	return nil
}

// daemon owns the state directory: it serves the API on the unix socket
// and the proxy on the network.
func daemon(socket string, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	domain := fs.String("domain", "", "proxy: root domain; box b answers at b.<domain> (required)")
	email := fs.String("acme-email", "", "proxy: contact address for Let's Encrypt")
	group := fs.String("group", "", "let this group use the API socket, like the docker group (root only if empty)")
	parse(fs, args, 0)
	*domain = strings.TrimSuffix(strings.ToLower(*domain), ".")
	if !validDomain.MatchString(*domain) {
		return fmt.Errorf("--domain must be a domain name like boxes.example.com, not %q", *domain)
	}

	// systemd's journal adds its own timestamps.
	opts := &slog.HandlerOptions{}
	if os.Getenv("JOURNAL_STREAM") != "" {
		opts.ReplaceAttr = func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))

	dir := env("CYAC_DIR", "/var/lib/cyac")
	rts, err := runtimes(dir)
	if err != nil {
		return err
	}
	if err := lock(dir); err != nil {
		return err
	}
	slog.Info("cyac daemon starting", "state_dir", dir, "conf", conf.Dir, "socket", socket)
	routes := filepath.Join(dir, "routes")
	px := &proxy.Server{Conf: conf.Dir, Routes: routes, Domain: *domain}
	if err := px.Reload(); err != nil {
		return err
	}
	// Boxes come and go with their units and their files; pick that up.
	go func() {
		for range time.Tick(5 * time.Second) {
			if err := px.Reload(); err != nil {
				slog.Error("proxy reload", "err", err)
			}
		}
	}()

	ln, err := listenUnix(socket, *group)
	if err != nil {
		return err
	}
	srv := &server.Server{
		Conf: conf.Dir, Routes: routes, Domain: *domain, RoutesChanged: px.Reload,
		Run: &run.Runner{State: dir, Runtimes: rts},
	}
	slog.Info("API listening", "socket", socket)
	go func() { log.Fatal(http.Serve(ln, srv.Handler())) }()

	// Certificates are only requested on demand, during the first TLS
	// handshake for an exposed host; HostPolicy refuses every other name.
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Email:      *email,
		HostPolicy: px.HostPolicy,
		Cache:      autocert.DirCache(filepath.Join(dir, "acme")),
	}
	// :80 answers ACME challenges and redirects everything else to HTTPS.
	go func() { log.Fatal(proxyServer(":80", m.HTTPHandler(nil)).ListenAndServe()) }()
	https := proxyServer(":443", px)
	https.TLSConfig = m.TLSConfig()
	slog.Info("proxy listening", "domain", *domain, "http", ":80", "https", ":443")
	go func() { log.Fatal(https.ListenAndServeTLS("", "")) }()

	// Boxes are their units' business; they outlive the daemon.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	slog.Info("exiting", "signal", <-sigs)
	return nil
}

// lock takes an exclusive lock on dir for the life of the process, so
// only one daemon serves it.
func lock(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	// Released when the process exits.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s is in use by another cyac daemon", dir)
	}
	return nil
}

var validDomain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// proxyServer has timeouts for slow or idle clients, but none on reading
// or writing bodies, which would cut off uploads, streams and websockets.
func proxyServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

// listenUnix opens the API socket, readable and writable by root only, or
// also by group if one is given. That is the only access control, so the
// group is as good as root, as with docker's.
func listenUnix(path, group string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	os.Remove(path) // stale socket from an earlier run; the store lock rules out a live one
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if group == "" {
		return ln, os.Chmod(path, 0o600)
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		ln.Close()
		return nil, err
	}
	gid, _ := strconv.Atoi(g.Gid)
	if err := os.Chown(path, 0, gid); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, os.Chmod(path, 0o660)
}

// parse handles flags before or after positional args and requires exactly n of them.
func parse(fs *flag.FlagSet, args []string, n int) []string {
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != n {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	return pos
}
