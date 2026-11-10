// cyac-generator is a systemd generator: it turns the conf files in
// /var/lib/cyac/conf into units. systemd runs it at boot and on every
// daemon-reload, as cyac-generator <normal> <early> <late>; it writes to
// <normal> only.
//
// cyac-generator -check <kind> <name> prints why an object can't be used
// and exits 1. Units for broken files run it, so the reason shows up in
// systemctl status and the journal.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/containmentzone/cyacrt/internal/conf"
	"github.com/containmentzone/cyacrt/internal/units"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "-check" {
		os.Exit(check(os.Args[2], os.Args[3]))
	}
	if len(os.Args) != 2 && len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: cyac-generator <normal-dir> [<early-dir> <late-dir>]\n       cyac-generator -check <kind> <name>")
		os.Exit(2)
	}
	if err := generate(os.Args[1]); err != nil {
		// A generator must never fail the boot; systemd logs this.
		fmt.Fprintln(os.Stderr, "cyac-generator:", err)
	}
}

func paths() (units.Paths, error) {
	self, err := os.Executable()
	if err != nil {
		return units.Paths{}, err
	}
	return units.Paths{
		Cyac:      filepath.Join(filepath.Dir(self), "cyac"),
		Generator: self,
		ConfDir:   conf.Dir,
	}, nil
}

func generate(dir string) error {
	p, err := paths()
	if err != nil {
		return err
	}
	c, probs, err := conf.Load(p.ConfDir)
	if err != nil {
		return err
	}
	return units.Write(dir, units.Generate(c, probs, p))
}

func check(kind, name string) int {
	_, probs, err := conf.Load(conf.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, pr := range probs {
		if pr.Kind == kind && pr.Name == name {
			fmt.Fprintln(os.Stderr, pr.Error())
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "%s.%s looks fine now; run systemctl daemon-reload\n", name, kind)
	return 1
}
