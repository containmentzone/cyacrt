// Command cyac manages boxes on a single host and serves the proxy in
// front of them. There is no daemon besides `cyac serve`: every other
// command edits the state directory directly under a file lock.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/containmentzone/cyacrt/internal/box"
	"github.com/containmentzone/cyacrt/internal/driver"
	"github.com/containmentzone/cyacrt/internal/network/bridge"
	"github.com/containmentzone/cyacrt/internal/proxy"
	"github.com/containmentzone/cyacrt/internal/runtime/firecracker"
	"github.com/containmentzone/cyacrt/internal/runtime/runc"
)

const usage = `usage: cyac <command> [args]

  new <name> --image <ref> [--size 10G] [--cpus 1] [--mem 1024] [--init cmd]
  start <name>
  stop <name>
  rm <name>
  ls
  resize <name> <size>
  exec <name> [cmd...]          (runc only; default: bash -l)
  expose <name> <port> <host> [--public]
  unexpose <host>
  serve [--listen :8080] | [--acme-email <e> --https :443 --http :80]

State lives in $CYAC_DIR (default /var/lib/cyac).
CYAC_RUNTIME=firecracker, firecracker-pvm or runc forces a runtime.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	dir := os.Getenv("CYAC_DIR")
	if dir == "" {
		dir = "/var/lib/cyac"
	}
	s := box.Store{
		Dir: dir,
		Net: bridge.Network{DNS: "1.1.1.1"},
		Runtimes: []driver.Runtime{
			firecracker.Runtime{Kernel: filepath.Join(dir, "vmlinux")},
			firecracker.Runtime{Kernel: filepath.Join(dir, "vmlinux-pvm"), PVM: true},
			runc.Runtime{},
		},
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd != "serve" && cmd != "ls" {
		if err := s.Lock(); err != nil {
			log.Fatal(err)
		}
	}
	if err := dispatch(s, cmd, args); err != nil {
		log.Fatal("cyac: ", err)
	}
}

func dispatch(s box.Store, cmd string, args []string) error {
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
		if *image == "" {
			return fmt.Errorf("--image is required")
		}
		if !box.ValidName(pos[0]) {
			return fmt.Errorf("invalid box name %q (lowercase letters, digits, dashes)", pos[0])
		}
		if _, err := s.Get(pos[0]); err == nil {
			return fmt.Errorf("box %q already exists", pos[0])
		}
		id, err := s.FreeID()
		if err != nil {
			return err
		}
		b := &box.Box{Name: pos[0], Image: *image, ID: id, CPUs: *cpus, MemMiB: *mem, Init: strings.Fields(*init)}
		log.Printf("seeding %s from %s", b.Name, b.Image)
		if err := s.Seed(b, bytes); err != nil {
			return err
		}
		if err := s.Put(b); err != nil {
			return err
		}
		return s.Start(b)

	case "start", "stop", "rm":
		b, err := s.Get(parse(fs, args, 1)[0])
		if err != nil {
			return err
		}
		switch cmd {
		case "start":
			return s.Start(b)
		case "stop":
			return s.Stop(b)
		default:
			return s.Remove(b)
		}

	case "ls":
		boxes, err := s.List()
		if err != nil {
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
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", b.Name, s.IP(b), s.Status(b), b.Image, strings.Join(routes, ", "))
		}
		return w.Flush()

	case "resize":
		pos := parse(fs, args, 2)
		b, err := s.Get(pos[0])
		if err != nil {
			return err
		}
		bytes, err := parseSize(pos[1])
		if err != nil {
			return err
		}
		return s.Resize(b, bytes)

	case "exec":
		if len(args) < 1 {
			return fmt.Errorf("usage: cyac exec <name> [cmd...]")
		}
		b, err := s.Get(args[0])
		if err != nil {
			return err
		}
		command := args[1:]
		if len(command) == 0 {
			command = []string{"/bin/bash", "-l"}
		}
		return s.Exec(b, command)

	case "expose":
		public := fs.Bool("public", false, "no token required")
		pos := parse(fs, args, 3)
		b, err := s.Get(pos[0])
		if err != nil {
			return err
		}
		port, err := strconv.Atoi(pos[1])
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("bad port %q", pos[1])
		}
		host := strings.ToLower(pos[2])
		if owner := routeOwner(s, host); owner != "" {
			return fmt.Errorf("%s is already exposed by %s", host, owner)
		}
		r := box.Route{Host: host, Port: port, Public: *public}
		token := ""
		if !*public {
			token = rand.Text()
			sum := sha256.Sum256([]byte(token))
			r.TokenHash = hex.EncodeToString(sum[:])
		}
		b.Routes = append(b.Routes, r)
		if err := s.Put(b); err != nil {
			return err
		}
		if token != "" {
			fmt.Printf("token: %s\nbrowser: https://%s/__cyac/login?token=%s\n", token, host, token)
		}
		return nil

	case "unexpose":
		host := strings.ToLower(parse(fs, args, 1)[0])
		boxes, err := s.List()
		if err != nil {
			return err
		}
		for _, b := range boxes {
			for i, r := range b.Routes {
				if r.Host == host {
					b.Routes = append(b.Routes[:i], b.Routes[i+1:]...)
					return s.Put(b)
				}
			}
		}
		return fmt.Errorf("%s is not exposed", host)

	case "serve":
		listen := fs.String("listen", ":8080", "plain HTTP listen address (when not using ACME)")
		email := fs.String("acme-email", "", "enable Let's Encrypt for exposed hosts")
		httpsAddr := fs.String("https", ":443", "HTTPS listen address (ACME mode)")
		httpAddr := fs.String("http", ":80", "HTTP listen address for ACME challenges (ACME mode)")
		parse(fs, args, 0)
		return serve(s, *listen, *email, *httpsAddr, *httpAddr)

	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	return nil
}

func serve(s box.Store, listen, email, httpsAddr, httpAddr string) error {
	srv := &proxy.Server{Store: s}
	if err := srv.Reload(); err != nil {
		return err
	}
	// The CLI changes routes by rewriting box records; pick them up every
	// few seconds, or immediately on SIGHUP.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-tick.C:
			case <-hup:
			}
			if err := srv.Reload(); err != nil {
				log.Printf("reload: %v", err)
			}
		}
	}()

	if email == "" {
		log.Printf("serving plain HTTP on %s", listen)
		return http.ListenAndServe(listen, srv)
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Email:      email,
		HostPolicy: srv.HostPolicy,
		Cache:      autocert.DirCache(filepath.Join(s.Dir, "acme")),
	}
	go func() { log.Fatal(http.ListenAndServe(httpAddr, m.HTTPHandler(nil))) }()
	https := &http.Server{Addr: httpsAddr, Handler: srv, TLSConfig: m.TLSConfig()}
	log.Printf("serving HTTPS on %s", httpsAddr)
	return https.ListenAndServeTLS("", "")
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

func routeOwner(s box.Store, host string) string {
	boxes, _ := s.List()
	for _, b := range boxes {
		for _, r := range b.Routes {
			if r.Host == host {
				return b.Name
			}
		}
	}
	return ""
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
