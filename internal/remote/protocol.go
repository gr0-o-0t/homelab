package remote

import (
	"fmt"
	"strings"
)

// Path is the websocket endpoint.
const Path = "/v1/run"

// LocalOnly are the commands the CLI always runs itself and the agent refuses
// to run for a client. The root command (bare `homelab`, `--gui`) is local
// too: it renders here, and the invocations its buttons exec are forwarded.
var LocalOnly = map[string]bool{
	"init": true, "join": true, "login": true, "logout": true, "version": true,
	"completion": true, "agent": true, "envshim": true, "edit": true,
}

// Mutating commands hold the server-wide lock for their whole run; every
// other command runs without waiting on it.
var Mutating = map[string]bool{
	"add": true, "setup": true, "up": true, "down": true, "start": true, "stop": true,
	"restart": true, "pull": true, "update": true, "delete": true, "enable": true,
	"disable": true, "reload": true, "backup": true, "restore": true, "prune": true, "edit": true,
}

// Handshake is the client's first frame.
type Handshake struct {
	Argv    []string `json:"argv"`
	TTY     bool     `json:"tty"`
	Rows    uint16   `json:"rows"`
	Cols    uint16   `json:"cols"`
	Version string   `json:"version"`
	Term    string   `json:"term"`
}

// control is every JSON text frame after the handshake, in either direction:
// client → resize, eof; server → stderr (pipes mode only), error, exit.
type control struct {
	Resize *[2]uint16 `json:"resize,omitempty"`
	EOF    bool       `json:"eof,omitempty"`
	Stderr []byte     `json:"stderr,omitempty"`
	Error  string     `json:"error,omitempty"`
	Exit   *int       `json:"exit,omitempty"`
}

// CommandName is the top-level command argv invokes: its first element that
// is not a flag. Every root flag that may precede it is a boolean (--config
// and --config-dir, the two taking a value, are refused before this is
// consulted). "" means the root command itself.
func CommandName(argv []string) string {
	for _, a := range argv {
		if a == "--" {
			return ""
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// validate refuses a handshake before anything runs.
func validate(h Handshake, agentVersion string) (int, error) {
	if majorMinor(h.Version) != majorMinor(agentVersion) {
		return 2, fmt.Errorf("homelab CLI %s and agent %s differ — run `homelab upgrade`", h.Version, agentVersion)
	}
	if len(h.Argv) == 0 {
		return 2, fmt.Errorf("no command given")
	}
	for _, a := range h.Argv {
		for _, f := range []string{"--config-dir", "--config"} {
			if a == f || strings.HasPrefix(a, f+"=") {
				return 2, fmt.Errorf("%s cannot be passed to the agent: it runs against its own state", f)
			}
		}
	}
	name := CommandName(h.Argv)
	if name == "" {
		return 2, fmt.Errorf("the root command runs on the local machine, not the agent")
	}
	if LocalOnly[name] {
		return 2, fmt.Errorf("`homelab %s` runs on the local machine, not the agent", name)
	}
	return 0, nil
}

// majorMinor is "1.4" for "v1.4.2"; a version with fewer than two parts
// ("dev") is compared whole.
func majorMinor(v string) string {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}
