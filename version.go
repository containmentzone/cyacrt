// Package cyacrt only holds cyac's release version, from the VERSION file
// at the repository root (go:embed can't reach above a package's
// directory). It follows PrideVer (https://pridever.org),
// PROUD.DEFAULT.SHAME; scripts/release.sh bumps it, and the Nix package
// reads it too.
package cyacrt

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

//go:embed VERSION
var version string

// Version is the version, with the commit appended as build metadata when
// the Go toolchain recorded one (go build in a git checkout, not Nix builds).
func Version() string {
	v := strings.TrimSpace(version)
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision":
			rev = s.Value[:min(12, len(s.Value))]
		case s.Key == "vcs.modified" && s.Value == "true":
			dirty = ".dirty"
		}
	}
	if rev == "" {
		return v
	}
	return v + "+" + rev + dirty
}
