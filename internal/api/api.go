// Package api is the wire format between the cyac CLI and the daemon:
// JSON over HTTP on a unix socket.
//
// There is no authentication beyond the socket's file permissions (root
// only by default, or root and the daemon's --group). Expect that to
// change before the socket is ever exposed beyond the host.
//
// Boxes are defined by files in the conf dir (package conf); the API
// only reads them.
//
//	GET    /v1/boxes                 → []Box
//	POST   /v1/boxes/{name}/start    systemctl start cybox@<name>
//	POST   /v1/boxes/{name}/stop     systemctl stop cybox@<name>
//	PUT    /v1/boxes/{name}/route    Expose → Exposed
//	DELETE /v1/boxes/{name}/route
//	POST   /v1/boxes/{name}/exec     Exec, then 101 and raw terminal bytes both ways
//	POST   /v1/boxes/{name}/console  101, then the console log's tail and new output; input goes to the console
//
// Errors come back as a non-2xx status with an Error body, except inside
// the exec and console streams.
package api

const DefaultSocket = "/run/cyac/cyac.sock"

type Box struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	Network string `json:"network"`
	IP      string `json:"ip"`
	Status  string `json:"status"` // "stopped" or "running (<runtime>)"
	Route   *Route `json:"route,omitempty"`
}

type Route struct {
	Host   string `json:"host"` // always <box>.<root domain>
	Port   int    `json:"port"`
	Public bool   `json:"public,omitempty"`
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

type Error struct {
	Error string `json:"error"`
}
