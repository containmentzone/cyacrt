// Command cyac is both the daemon (`cyac daemon`), which owns the state
// directory and serves the proxy, and the CLI, which talks to the daemon
// over a unix socket.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/containmentzone/cyacrt/internal/api"
	"github.com/containmentzone/cyacrt/internal/box"
	"github.com/containmentzone/cyacrt/internal/client"
	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/network/bridge"
	"github.com/containmentzone/cyacrt/internal/proxy"
	"github.com/containmentzone/cyacrt/internal/runtime/firecracker"
	"github.com/containmentzone/cyacrt/internal/runtime/runc"
	"github.com/containmentzone/cyacrt/internal/server"
)

const usage = `usage: cyac <command> [args]

  new <name> --image <ref> [--size 10G] [--cpus 1] [--mem 1024] [--init cmd]
  start <name>
  stop <name>
  rm <name>
  ls
  resize <name> <size>
  exec [--root] <name> [cmd...] (default: bash -l, as the image's login user; VMs need sshd)
  console <name>                attach to the console; Ctrl-] or Enter ~ . detaches
  expose <name> <port> <host> [--public]
  unexpose <host>

  daemon [--listen :8080] | [--acme-email <e> --https :443 --http :80]

The CLI talks to the daemon on $CYAC_SOCKET (default /run/cyac/cyac.sock).
The daemon keeps state in $CYAC_DIR (default /var/lib/cyac).
CYAC_RUNTIME=firecracker, firecracker-pvm or runc forces a runtime.
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
	if cmd == "daemon" {
		err = daemon(socket, args)
	} else {
		err = dispatch(client.New(socket), cmd, args)
	}
	if err != nil {
		log.Fatal("cyac: ", err)
	}
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
	case "new":
		image := fs.String("image", "", "OCI image to seed the disk from")
		size := fs.String("size", "10G", "disk size")
		cpus := fs.Int("cpus", 1, "vCPUs")
		mem := fs.Int("mem", 1024, "memory in MiB")
		init := fs.String("init", "", "PID 1 command, space-separated (default: the image's ENTRYPOINT+CMD)")
		pos := parse(fs, args, 1)
		bytes, err := parseSize(*size)
		if err != nil {
			return err
		}
		req := api.NewBox{Name: pos[0], Image: *image, SizeBytes: bytes, CPUs: *cpus, MemMiB: *mem, Init: strings.Fields(*init)}
		start := time.Now()
		b, err := c.Create(req, func(msg string) {
			fmt.Fprintf(os.Stderr, "[%4.0fs] %s\n", time.Since(start).Seconds(), msg)
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[%4.0fs] %s is %s at %s\n", time.Since(start).Seconds(), b.Name, b.Status, b.IP)
		return nil

	case "start", "stop":
		return c.Do("POST", "/v1/boxes/"+parse(fs, args, 1)[0]+"/"+cmd, nil, nil)

	case "rm":
		return c.Do("DELETE", "/v1/boxes/"+parse(fs, args, 1)[0], nil, nil)

	case "ls":
		parse(fs, args, 0)
		var boxes []api.Box
		if err := c.Do("GET", "/v1/boxes", nil, &boxes); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tIP\tSTATUS\tIMAGE\tROUTES")
		for _, b := range boxes {
			var routes []string
			for _, r := range b.Routes {
				vis := "token"
				if r.Public {
					vis = "public"
				}
				routes = append(routes, fmt.Sprintf("%s→:%d (%s)", r.Host, r.Port, vis))
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", b.Name, b.IP, b.Status, b.Image, strings.Join(routes, ", "))
		}
		return w.Flush()

	case "resize":
		pos := parse(fs, args, 2)
		bytes, err := parseSize(pos[1])
		if err != nil {
			return err
		}
		return c.Do("POST", "/v1/boxes/"+pos[0]+"/resize", api.Resize{SizeBytes: bytes}, nil)

	case "exec":
		root := fs.Bool("root", false, "run as root instead of the image's login user")
		fs.Parse(args) // flags go before the name; the command is passed through as is
		if fs.NArg() < 1 {
			return errors.New("usage: cyac exec [--root] <name> [cmd...]")
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
		pos := parse(fs, args, 3)
		port, err := strconv.Atoi(pos[1])
		if err != nil {
			return fmt.Errorf("bad port %q", pos[1])
		}
		var tok api.Token
		if err := c.Do("POST", "/v1/boxes/"+pos[0]+"/routes", api.Expose{Host: pos[2], Port: port, Public: *public}, &tok); err != nil {
			return err
		}
		if tok.Token != "" {
			host := strings.ToLower(pos[2])
			fmt.Printf("token: %s\nbrowser: https://%s/__cyac/login?token=%s\n", tok.Token, host, tok.Token)
		}
		return nil

	case "unexpose":
		return c.Do("DELETE", "/v1/routes/"+parse(fs, args, 1)[0], nil, nil)

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
	listen := fs.String("listen", ":8080", "proxy: plain HTTP listen address (when not using ACME)")
	email := fs.String("acme-email", "", "proxy: enable Let's Encrypt for exposed hosts")
	httpsAddr := fs.String("https", ":443", "proxy: HTTPS listen address (ACME mode)")
	httpAddr := fs.String("http", ":80", "proxy: HTTP listen address for ACME challenges (ACME mode)")
	parse(fs, args, 0)

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
	s := box.Store{
		Dir: dir,
		Net: bridge.Network{DNS: "1.1.1.1"},
		Runtimes: []driver.Runtime{
			firecracker.Runtime{Kernel: filepath.Join(dir, "vmlinux")},
			firecracker.Runtime{Kernel: filepath.Join(dir, "vmlinux-pvm"), PVM: true},
			runc.Runtime{},
		},
	}
	if err := s.Lock(); err != nil {
		return err
	}
	slog.Info("cyac daemon starting", "state_dir", dir, "socket", socket, "CYAC_RUNTIME", os.Getenv("CYAC_RUNTIME"))
	for _, rt := range s.Runtimes {
		if err := rt.Available(); err != nil {
			slog.Info("runtime unavailable", "runtime", rt.Name(), "reason", err)
		} else {
			slog.Info("runtime available", "runtime", rt.Name())
		}
	}
	boxes, err := s.List()
	if err != nil {
		return err
	}
	for _, b := range boxes {
		slog.Info("box", "box", b.Name, "status", s.Status(b), "ip", s.IP(b), "routes", len(b.Routes))
	}
	px := &proxy.Server{Store: s}
	if err := px.Reload(); err != nil {
		return err
	}

	ln, err := listenUnix(socket)
	if err != nil {
		return err
	}
	srv := &server.Server{Store: s, RoutesChanged: px.Reload}
	slog.Info("API listening", "socket", socket)
	go func() { log.Fatal(http.Serve(ln, srv.Handler())) }()
	go srv.Watch(2 * time.Second)

	if *email == "" {
		slog.Info("proxy listening", "addr", *listen, "tls", false)
		return http.ListenAndServe(*listen, px)
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Email:      *email,
		HostPolicy: px.HostPolicy,
		Cache:      autocert.DirCache(filepath.Join(dir, "acme")),
	}
	go func() { log.Fatal(http.ListenAndServe(*httpAddr, m.HTTPHandler(nil))) }()
	https := &http.Server{Addr: *httpsAddr, Handler: px, TLSConfig: m.TLSConfig()}
	slog.Info("proxy listening", "addr", *httpsAddr, "tls", "acme", "acme_http", *httpAddr)
	return https.ListenAndServeTLS("", "")
}

// listenUnix opens the API socket, readable and writable by root only.
// That is the only access control for now.
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	os.Remove(path) // stale socket from an earlier run; the store lock rules out a live one
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return ln, os.Chmod(path, 0o600)
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

// parseSize accepts plain bytes or a K/M/G/T suffix (powers of 1024).
func parseSize(v string) (int64, error) {
	mult := int64(1)
	if i := strings.IndexAny(v, "KMGTkmgt"); i == len(v)-1 {
		mult = 1 << (10 * (strings.IndexByte("KMGT", byte(strings.ToUpper(v[i:])[0])) + 1))
		v = v[:i]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad size %q", v)
	}
	return n * mult, nil
}
