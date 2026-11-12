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
	"completion": true, "agent": true, "agent-seed": true, "envshim": true, "edit": true,
}

// Internal commands are plumbing the agent runs itself, never for a client.
var Internal = map[string]bool{"agent": true, "agent-seed": true, "envshim": true}

// Mutating commands (canonical top-level names) hold the server-wide lock
// for their whole run, except the ReadOnly subcommands under them; every
// other command runs without waiting on it. See Locks.
var Mutating = map[string]bool{
	"add": true, "setup": true, "up": true, "down": true, "start": true, "stop": true,
	"restart": true, "pull": true, "update": true, "delete": true, "enable": true,
	"disable": true, "reload": true, "backup": true, "restore": true, "prune": true,
	"ext": true, "cf": true, "new": true, "cluster": true, "files": true, "upgrade": true,
}

// ReadOnly are the canonical paths of observing commands that never take the
// lock: subcommands under a Mutating command (their siblings, cf route
// add/rm and ext enable/disable, take it), and backup-list, the hidden
// lock-free twin of `backup --list` the front ends poll on every refresh.
var ReadOnly = map[string]bool{
	"cf status": true, "cf logs": true,
	"ext list": true, "ext status": true, "ext logs": true,
	"files get":   true,
	"backup-list": true,
}

// LockedSub are the canonical paths of subcommands that take the lock
// although their top-level command does not: `images load` writes to the
// engine, while `images` only reads.
var LockedSub = map[string]bool{"images load": true}

// Detached are the commands (canonical top-level names) that run to
// completion when their client disconnects: stopping a backup midway leaves
// a service scaled to zero, and stopping a restore can kill it in the
// middle of replacing a volume; stopping an upgrade can leave the cluster
// half rolled, a worker's agent mid-replacement. The server stops streaming
// to the vanished client, copies the output to the agent log, and keeps the
// lock until the command exits.
var Detached = map[string]bool{"backup": true, "restore": true, "upgrade": true}

// DetachOnDisconnect reports whether the canonical command path survives
// its client going away.
func DetachOnDisconnect(path []string) bool {
	return len(path) > 0 && Detached[path[0]]
}

// refusedFlags may not appear in a forwarded argv: they would choose the
// agent's state. They are refused anywhere, except after exec's service
// name, where the argv is the container command's own (see refusalEnd).
var refusedFlags = []string{"--config-dir", "--config"}

// ExecValueFlags are exec's flags that take their value as the next
// argument (the long forms and the shorthand letters), and the root
// persistent ones that do. refusalEnd skips their values to find the
// service name; cmd's tests pin this list against exec's flag set.
var ExecValueFlags = map[string]bool{
	"--user": true, "--workdir": true, "--env": true, "--container": true,
	"--config": true, "--config-dir": true,
	"-u": true, "-w": true, "-e": true, "-c": true,
}

// refusalEnd is the length of the argv prefix refusedFlags are checked in:
// all of it, except for exec, whose flags stop at the service name
// (SetInterspersed(false)) — everything after it is the container command's
// argv and reaches the child verbatim. Unknown spellings err toward
// checking more: a value-taking flag this misses would end the prefix at
// its value, so ExecValueFlags must stay complete.
func refusalEnd(argv []string) int {
	i := 0
	for ; i < len(argv); i++ { // up to the command name
		if argv[i] == "--" {
			return len(argv)
		}
		if !strings.HasPrefix(argv[i], "-") {
			break
		}
	}
	if i == len(argv) || argv[i] != "exec" {
		return len(argv)
	}
	for i++; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			return min(i+2, len(argv)) // the service follows; check it too
		case !strings.HasPrefix(a, "-") || a == "-":
			return i + 1 // the service name
		case strings.Contains(a, "="):
		case strings.HasPrefix(a, "--"):
			if ExecValueFlags[a] {
				i++
			}
		default: // a shorthand cluster (-Tu): the first value-taking letter
			// takes the rest of the cluster, or the next argument
			for j := 1; j < len(a); j++ {
				if ExecValueFlags["-"+a[j:j+1]] {
					if j == len(a)-1 {
						i++
					}
					break
				}
			}
		}
	}
	return len(argv)
}

// Resolver maps argv to the canonical command path it invokes (aliases
// resolved, e.g. ["rm","x"] → ["delete"]); ok is false for an unknown
// command. An empty path is the root command.
type Resolver func(argv []string) (path []string, ok bool)

// refuseAll is the resolver of a server configured without one: failing
// closed, since taking the first word at face value would let aliases
// (`rm` for `delete`) past the lock.
func refuseAll([]string) ([]string, bool) { return nil, false }

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

// AcrossVersions are the commands (canonical top-level names) an agent runs
// for a client whose major.minor differs: upgrade, invoked by a CLI newer
// than the agent (the normal upgrade path), and version, which is how
// upgrade checks an agent. Besides these, only the Internal paths a server
// allows (a worker agent's `agent self-replace`, which the leader sends
// while upgrading a worker still on an older version) cross a version
// difference.
var AcrossVersions = map[string]bool{"upgrade": true, "version": true}

// AgentAnswers are LocalOnly commands the agent still runs when a client
// asks it directly (a CLI typing `homelab version` runs it locally; upgrade
// asks an agent for its own).
var AgentAnswers = map[string]bool{"version": true}

// validate refuses a handshake before anything runs, and otherwise returns
// the canonical command path (never empty). allowInternal are the canonical
// paths of Internal commands this server runs for a client.
func validate(h Handshake, agentVersion string, resolve Resolver, allowInternal map[string]bool) ([]string, int, error) {
	mismatch := majorMinor(h.Version) != majorMinor(agentVersion)
	refuseMismatch := func() ([]string, int, error) {
		return nil, 2, fmt.Errorf("homelab CLI %s and agent %s differ — run `homelab upgrade`", h.Version, agentVersion)
	}
	// The command is only resolved for a mismatched client when it names
	// one that may cross: the rest are refused before the agent's cobra
	// tree sees them.
	// A server allowing Internal paths (a worker agent) resolves those too:
	// the leader upgrading a worker one minor behind sends `agent
	// self-replace` across the difference (R55).
	mayCross := func(name string) bool {
		return AcrossVersions[name] || (Internal[name] && len(allowInternal) > 0)
	}
	if mismatch && !mayCross(CommandName(h.Argv)) {
		return refuseMismatch()
	}
	if len(h.Argv) == 0 {
		return nil, 2, fmt.Errorf("no command given")
	}
	for _, a := range h.Argv[:refusalEnd(h.Argv)] {
		for _, f := range refusedFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return nil, 2, fmt.Errorf("%s cannot be passed to the agent", f)
			}
		}
	}
	path, ok := resolve(h.Argv)
	if !ok {
		return nil, 2, fmt.Errorf("unknown command %q", CommandName(h.Argv))
	}
	if len(path) == 0 {
		return nil, 2, fmt.Errorf("the root command runs on the local machine, not the agent")
	}
	name := path[0]
	// The resolved name decides, not the first word: an alias must not
	// smuggle another command across the version check.
	if mismatch && !AcrossVersions[name] && !allowInternal[strings.Join(path, " ")] {
		return refuseMismatch()
	}
	if allowInternal[strings.Join(path, " ")] {
		return path, 0, nil
	}
	// help, completion and cobra's __complete* run on the client
	// (cmd.shouldForward); nothing forwards them.
	if (LocalOnly[name] && !AgentAnswers[name]) || name == "help" || name == "completion" || strings.HasPrefix(name, "__") {
		return nil, 2, fmt.Errorf("`homelab %s` runs on the local machine, not the agent", name)
	}
	if Internal[name] {
		return nil, 2, fmt.Errorf("`homelab %s` is internal to the agent and cannot be run remotely", name)
	}
	return path, 0, nil
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

// Locks reports whether the canonical command path takes the lock. It looks
// at the path only, never at flags: the server cannot parse an argv the way
// cobra will in the child, so a flag-based exemption could be spelled to let
// a mutating run through unlocked.
func Locks(path []string) bool {
	if len(path) == 0 || ReadOnly[strings.Join(path, " ")] {
		return false
	}
	return Mutating[path[0]] || LockedSub[strings.Join(path, " ")]
}
