package client

import "testing"

func TestEscaper(t *testing.T) {
	for _, tc := range []struct {
		in, out string
		detach  bool
	}{
		{"ls\r", "ls\r", false},
		{"~.", "", true},
		{"ls\r~.", "ls\r", true},
		{"a~.", "a~.", false}, // not at the start of a line
		{"~~", "~", false},
		{"~x", "~x", false},
		{"ab\x1dcd", "ab", true},
		{"\x03\x04", "\x03\x04", false}, // Ctrl-C and Ctrl-D reach the box
	} {
		e := escaper{lineStart: true}
		out, detach := e.filter([]byte(tc.in))
		if string(out) != tc.out || detach != tc.detach {
			t.Errorf("%q: got %q, %v; want %q, %v", tc.in, out, detach, tc.out, tc.detach)
		}
	}

	// The sequence can arrive split across reads.
	e := escaper{lineStart: true}
	if out, detach := e.filter([]byte("x\r~")); string(out) != "x\r" || detach {
		t.Fatalf("first read: %q, %v", out, detach)
	}
	if _, detach := e.filter([]byte(".")); !detach {
		t.Fatal("split ~. didn't detach")
	}
}
