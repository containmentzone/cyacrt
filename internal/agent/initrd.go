package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// WriteInitrd writes the initramfs a VM boots with: the cyac-init binary
// at init as /init, and cfg as /cyac.json. The kernel unpacks it into a
// tmpfs and runs /init, which mounts the box's disk (see cmd/cyac-init).
func WriteInitrd(w io.Writer, init string, cfg Config) error {
	bin, err := os.ReadFile(init)
	if err != nil {
		return err
	}
	conf, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	entries := []struct {
		name         string
		mode         uint32
		major, minor int
		data         []byte
	}{
		{name: "dev", mode: 0o040755},
		// The kernel opens /dev/console for init's stdio before init runs.
		{name: "dev/console", mode: 0o020600, major: 5, minor: 1},
		{name: "newroot", mode: 0o040755},
		{name: "init", mode: 0o100755, data: bin},
		{name: "cyac.json", mode: 0o100644, data: conf},
		{name: "TRAILER!!!"},
	}
	for i, e := range entries {
		// The "newc" cpio format: a header of hex fields, then the
		// NUL-terminated name and the data, each padded to 4 bytes.
		hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			i+1, e.mode, 0, 0, 1, 0, len(e.data), 0, 0, e.major, e.minor, len(e.name)+1, 0)
		name := hdr + e.name + "\x00"
		if _, err := io.WriteString(w, name+pad(len(name))); err != nil {
			return err
		}
		if _, err := w.Write(e.data); err != nil {
			return err
		}
		if _, err := io.WriteString(w, pad(len(e.data))); err != nil {
			return err
		}
	}
	return nil
}

func pad(n int) string { return "\x00\x00\x00"[:(4-n%4)%4] }
