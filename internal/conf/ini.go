package conf

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// section is one [Name] block of an ini file, its entries in file order.
// Keys may repeat; that is how lists are written (Attach=, Secret=, Env=).
type section struct {
	name    string
	entries []entry
}

type entry struct {
	key, value string
	line       int
}

// parseINI reads the subset of systemd's unit file syntax cyac uses:
// [Section] headers, Key=Value lines, blank lines, and comments starting
// with # or ;. A trailing backslash continues a value on the next line.
// Section and key names are case sensitive, as in systemd.
func parseINI(r io.Reader) ([]section, error) {
	var (
		secs []section
		sc   = bufio.NewScanner(r)
		n    int
	)
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		start := n
		for strings.HasSuffix(line, `\`) && sc.Scan() {
			n++
			line = strings.TrimSpace(strings.TrimSuffix(line, `\`)) + " " + strings.TrimSpace(sc.Text())
		}
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[':
			if !strings.HasSuffix(line, "]") || len(line) < 3 {
				return nil, fmt.Errorf("line %d: bad section header %q", start, line)
			}
			secs = append(secs, section{name: line[1 : len(line)-1]})
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf("line %d: want Key=Value, got %q", start, line)
			}
			if len(secs) == 0 {
				return nil, fmt.Errorf("line %d: %s= outside a section", start, strings.TrimSpace(k))
			}
			s := &secs[len(secs)-1]
			s.entries = append(s.entries, entry{strings.TrimSpace(k), strings.TrimSpace(v), start})
		}
	}
	return secs, sc.Err()
}

// fields is the view a primitive's parser has of one section: it takes the
// keys it knows, and whatever is left over is an error, so a typo never
// silently means "unset". Errors stick: the parser reads every key, and
// done reports the first error.
type fields struct {
	section string
	byKey   map[string][]entry
	err     error
}

// fail records err, unless an earlier error is recorded or err is nil.
func (f *fields) fail(err error) {
	if f.err == nil {
		f.err = err
	}
}

func newFields(s section) *fields {
	f := &fields{section: s.name, byKey: map[string][]entry{}}
	for _, e := range s.entries {
		f.byKey[e.key] = append(f.byKey[e.key], e)
	}
	return f
}

// one takes key, which may appear at most once.
func (f *fields) one(key string) string {
	es := f.byKey[key]
	delete(f.byKey, key)
	if len(es) == 0 {
		return ""
	}
	if len(es) > 1 {
		f.fail(fmt.Errorf("line %d: [%s] %s= given more than once", es[1].line, f.section, key))
	}
	return es[0].value
}

// int takes key as a number from lo to hi; unset is 0, or an error if
// required.
func (f *fields) int(key string, lo, hi int, required bool) int {
	v, err := intIn(f.one(key), key, lo, hi, required)
	f.fail(err)
	return v
}

// size takes key as a size in MiB (see sizeMB); unset is 0.
func (f *fields) size(key string) int {
	v, err := sizeMB(f.one(key), key)
	f.fail(err)
	return v
}

// bool takes key as yes or no; unset is no.
func (f *fields) bool(key string) bool {
	v, err := boolean(f.one(key), key)
	f.fail(err)
	return v
}

// all takes every value of key, in file order.
func (f *fields) all(key string) []string {
	var vs []string
	for _, e := range f.byKey[key] {
		vs = append(vs, e.value)
	}
	delete(f.byKey, key)
	return vs
}

// done reports the first error, or else the first key nobody took.
func (f *fields) done() error {
	if f.err != nil {
		return f.err
	}
	var first *entry
	for _, es := range f.byKey {
		if first == nil || es[0].line < first.line {
			first = &es[0]
		}
	}
	if first != nil {
		return fmt.Errorf("line %d: [%s] unknown key %s=", first.line, f.section, first.key)
	}
	return nil
}
