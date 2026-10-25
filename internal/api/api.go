// Package api is the wire format between the cyac CLI and the daemon:
// JSON over HTTP on a unix socket.
//
// There is no authentication beyond the socket's file permissions (root
// only by default, or root and the daemon's --group). Expect that to
// change before the socket is ever exposed beyond the host.
//
//	GET    /v1/boxes                 → []Box
//	POST   /v1/boxes                 NewBox → a stream of Progress, one JSON object per line
//	POST   /v1/boxes/{name}/start
//	POST   /v1/boxes/{name}/stop
//	POST   /v1/boxes/{name}/resize   Resize
//	DELETE /v1/boxes/{name}
//	PUT    /v1/boxes/{name}/route    Expose → Exposed
//	DELETE /v1/boxes/{name}/route
//	POST   /v1/boxes/{name}/exec     Exec, then 101 and raw terminal bytes both ways
//	POST   /v1/boxes/{name}/console  101, then the console log's tail and new output; input goes to the console
//	POST   /v1/prune                 → Pruned
//
// Errors come back as a non-2xx status with an Error body, except inside
// a Progress stream, where the last object carries them.
package api

const DefaultSocket = "/run/cyac/cyac.sock"

type Box struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	IP     string `json:"ip"`
	Status string `json:"status"` // "stopped" or "running (<runtime>)"
	Route  *Route `json:"route,omitempty"`
}

type Route struct {
	Host   string `json:"host"` // always <box>.<root domain>
	Port   int    `json:"port"`
	Public bool   `json:"public,omitempty"`
}

type NewBox struct {
	Name      string   `json:"name"`
	Image     string   `json:"image"`
	SizeBytes int64    `json:"size_bytes"`
	CPUs      int      `json:"cpus"`
	MemMiB    int      `json:"mem_mib"`
	Init      []string `json:"init,omitempty"` // empty: the image's ENTRYPOINT+CMD
	Mode      string   `json:"mode,omitempty"` // "init", "command", "pause", or empty to pick from the image
}

// Progress is one line of a streamed reply. The last line has Box or
// Error set.
type Progress struct {
	Message string `json:"message,omitempty"`
	Box     *Box   `json:"box,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Resize struct {
	SizeBytes int64 `json:"size_bytes"`
}

type Expose struct {
	Port   int  `json:"port"`
	Public bool `json:"public,omitempty"`
}

// Exposed answers Expose. Token is only set, and only ever shown, when a
// non-public route is created.
type Exposed struct {
	Host  string `json:"host"`
	Token string `json:"token,omitempty"`
}

// Exec starts a command on a pty in the box. Rows and Cols set the initial
// terminal size. It runs as the image's login user, if it named one,
// unless Root is set.
type Exec struct {
	Args []string `json:"args"`
	Root bool     `json:"root,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
	Term string   `json:"term,omitempty"` // the client's $TERM
}

// ExecUpgrade is the Upgrade token for the exec stream.
const ExecUpgrade = "cyac-exec"

// ConsoleUpgrade is the Upgrade token for the console stream.
const ConsoleUpgrade = "cyac-console"

// Pruned answers a prune: the disk space the removed base disks took.
type Pruned struct {
	Bytes int64 `json:"bytes"`
}

type Error struct {
	Error string `json:"error"`
}
