// Package sh runs host commands, folding their output into errors.
package sh

import (
	"fmt"
	"os/exec"
	"strings"
)

// Run executes a command and folds its output into the error.
func Run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
