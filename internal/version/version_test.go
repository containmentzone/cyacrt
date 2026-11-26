package version

import (
	"regexp"
	"testing"
)

// scripts/release.sh and the Nix package both expect exactly PROUD.DEFAULT.SHAME.
func TestVersionFile(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\n?$`).MatchString(version) {
		t.Fatalf("VERSION is %q, want PROUD.DEFAULT.SHAME", version)
	}
}
