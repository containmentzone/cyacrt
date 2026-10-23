package main

import (
	"bufio"
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"github.com/containmentzone/cyacrt/internal/agent"
)

func TestSession(t *testing.T) {
	for _, tc := range []struct {
		script, want string
		status       int
	}{
		{"read l; echo got $l; exit 3", "got hello", 3},
		{"kill -9 $$", "", 137},
	} {
		host, guest := net.Pipe()
		go session(guest)
		if _, err := host.Write([]byte(`{"args":["sh","-c","` + tc.script + `"],"rows":24,"cols":80}` + "\nhello\n")); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		r := bufio.NewReader(host)
		for {
			typ, p, err := agent.ReadFrame(r)
			if err != nil {
				t.Fatalf("%q: %v (output %q)", tc.script, err, out.String())
			}
			if typ == agent.Output {
				out.Write(p)
				continue
			}
			if got := int(int32(binary.BigEndian.Uint32(p))); got != tc.status {
				t.Errorf("%q: status %d, want %d", tc.script, got, tc.status)
			}
			break
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%q: output %q, want %q", tc.script, out.String(), tc.want)
		}
		host.Close()
	}
}
