// Package api is the wire format between the cyac CLI and the daemon:
// JSON over HTTP on a unix socket.
//
// There is no authentication beyond the socket's file permissions (root
// only by default). Expect that to change before the socket is ever
// exposed beyond the host.
//
//	GET    /v1/boxes                 → []Box
//	POST   /v1/boxes                 NewBox → a stream of Progress, one JSON object per line
//	POST   /v1/boxes/{name}/start
//	POST   /v1/boxes/{name}/stop
//	POST   /v1/boxes/{name}/resize   Resize
//	DELETE /v1/boxes/{name}
//	POST   /v1/boxes/{name}/routes   Expose → Token
//	DELETE /v1/routes/{host}
//	POST   /v1/boxes/{name}/exec     Exec, then 101 and raw terminal bytes both ways
//	POST   /v1/boxes/{name}/console  101, then the console log's tail and new output; input goes to the console
//
// Errors come back as a non-2xx status with an Error body, except inside
// a Progress stream, where the last object carries them.
package api

const DefaultSocket = "/run/cyac/cyac.sock"

type Box struct {
	Name   string  `json:"name"`
	Image  string  `json:"image"`
	IP     string  `json:"ip"`
	Status string  `json:"status"` // "stopped" or "running (<runtime>)"
	Routes []Route `json:"routes,omitempty"`
}

type Route struct {
	Host   string `json:"host"`
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
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Public bool   `json:"public,omitempty"`
}

// Token is returned once, when a non-public route is created.
type Token struct {
	Token string `json:"token,omitempty"`
}

// Exec starts a command on a pty in the box. Rows and Cols set the initial
// terminal size.
type Exec struct {
	Args []string `json:"args"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

// ExecUpgrade is the Upgrade token for the exec stream.
const ExecUpgrade = "cyac-exec"

// ConsoleUpgrade is the Upgrade token for the console stream.
const ConsoleUpgrade = "cyac-console"

type Error struct {
	Error string `json:"error"`
}
