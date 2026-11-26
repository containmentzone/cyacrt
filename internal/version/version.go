// Package version is cyac's release version. It follows PrideVer
// (https://pridever.org), PROUD.DEFAULT.SHAME; scripts/release.sh bumps
// the VERSION file next to this one, which the Nix package reads too.
package version

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

//go:embed VERSION
var version string

// String is the version, with the commit appended as build metadata when
// the Go toolchain recorded one (go build in a git checkout, not Nix builds).
func String() string {
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
