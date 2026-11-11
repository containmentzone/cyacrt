package render

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containmentzone/cyacrt/internal/conf"
)

var update = flag.Bool("update", false, "rewrite testdata/plans.golden")

func dump(b *strings.Builder, title string, p Plan) {
	fmt.Fprintf(b, "==> %s <==\n", title)
	for _, part := range []struct {
		name  string
		steps []Step
	}{{"up", p.Up}, {"down", p.Down}} {
		b.WriteString(part.name + ":\n")
		for _, s := range part.steps {
			b.WriteString("  " + strings.Join(s.Args, " ") + "\n")
			if s.Stdin != "" {
				b.WriteString("    <<<\n")
				for _, l := range strings.Split(strings.TrimSuffix(s.Stdin, "\n"), "\n") {
					b.WriteString("    " + l + "\n")
				}
				b.WriteString("    >>>\n")
			}
		}
	}
	b.WriteString("\n")
}

// TestPlans renders every object in testdata/conf and compares the
// result with testdata/plans.golden, which is meant to be read in review.
func TestPlans(t *testing.T) {
	c, probs, err := conf.Load("testdata/conf")
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	var b strings.Builder
	for _, name := range conf.Sorted(c.Gateways) {
		dump(&b, "cygateway "+name, Gateway(c.Gateways[name]))
	}
	for _, name := range conf.Sorted(c.Networks) {
		dump(&b, "cynetwork "+name, Network(c.Networks[name]))
	}
	for _, name := range conf.Sorted(c.Services) {
		dump(&b, "cyservice "+name, Service(c, c.Services[name]))
	}
	for _, name := range conf.Sorted(c.Boxes) {
		dump(&b, "cybox "+name+" (VM)", Box(c, c.Boxes[name], true))
		dump(&b, "cybox "+name+" (runc)", Box(c, c.Boxes[name], false))
		fmt.Fprintf(&b, "==> cybox %s networkd <==\n%s\n", name, Networkd(c, c.Boxes[name]))
	}

	golden := filepath.Join("testdata", "plans.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("output differs from %s; run go test ./internal/render -update and review the diff", golden)
	}
}

// Interface names must fit Linux's 15 bytes at the largest indexes.
func TestNameLengths(t *testing.T) {
	n := &conf.Network{Index: conf.MaxNetworkIndex}
	s := &conf.Service{Index: conf.MaxLinkIndex}
	b := &conf.Box{Address: n.Prefix().Addr().Next()}
	for _, name := range []string{
		Bridge(n), Port(b),
		fmt.Sprintf("cys%dn%d", s.Index, n.Index),
		fmt.Sprintf("cynl%d", n.Index),
		fmt.Sprintf("cygw%d", conf.MaxLinkIndex),
		fmt.Sprintf("cywg%d", conf.MaxLinkIndex),
	} {
		if len(name) > 15 {
			t.Errorf("%q is longer than 15 bytes", name)
		}
	}
}

// TestNftSyntax checks every nft script in the plans with nft -c, which
// parses without touching the ruleset. Skipped where nft isn't installed.
func TestNftSyntax(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	c, _, err := conf.Load("testdata/conf")
	if err != nil {
		t.Fatal(err)
	}
	var plans []Plan
	for _, g := range c.Gateways {
		plans = append(plans, Gateway(g))
	}
	for _, n := range c.Networks {
		plans = append(plans, Network(n))
	}
	for _, s := range c.Services {
		plans = append(plans, Service(c, s))
	}
	for _, b := range c.Boxes {
		plans = append(plans, Box(c, b, true))
	}
	for _, p := range plans {
		for _, s := range p.Up {
			if s.Stdin == "" {
				continue
			}
			cmd := exec.Command("nft", "-c", "-f", "-")
			cmd.Stdin = strings.NewReader(s.Stdin)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("nft -c: %v\n%s\n%s", err, out, s.Stdin)
			}
		}
	}
}
