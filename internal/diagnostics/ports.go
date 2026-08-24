package diagnostics

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
)

// A published port that is already taken by something outside this stack makes
// `docker compose up` fail with "failed to bind host port …: address already in
// use", and only for the one container — the rest of the stack comes up, so the
// failure is easy to miss in the middle of compose's output. Everything needed
// to predict it is on the host already: the compose files say which ports get
// published, and a bind attempt says whether they are free.

// portBinding is one published host port, resolved.
type portBinding struct {
	hostIP    string // "" means all interfaces
	hostPort  string
	proto     string // "tcp" or "udp"
	container string // container_name that will try to bind it
	origin    string // compose file it came from, for the failure message
}

func (b portBinding) label() string {
	if b.hostIP == "" {
		return b.hostPort + "/" + b.proto
	}
	return b.hostIP + ":" + b.hostPort + "/" + b.proto
}

// RunPortChecks reports published host ports that are occupied by something
// other than the container meant to own them.
//
// env resolves the ${VAR:-default} references in the compose files — pass the
// same map the lifecycle commands hand to compose, or the defaults win and the
// check reports on ports the stack was never going to use. profiles are the
// active compose profiles: a port belonging to a disabled extension is not a
// collision, because that container never starts.
func RunPortChecks(dir string, env map[string]string, profiles []string, dc *docker.Client) CheckGroup {
	files := []string{run.CoreComposeFile(dir)}
	svcFiles, _ := filepath.Glob(filepath.Join(dir, "services", "*", "docker-compose.yml"))
	sort.Strings(svcFiles)
	return RunPortChecksForFiles(append(files, svcFiles...), env, profiles, dc)
}

// RunPortChecksForFiles is RunPortChecks scoped to specific compose files, for
// the lifecycle commands: `homelab up jellyfin` should not report a collision
// that belongs to some other service it is not starting.
func RunPortChecksForFiles(files []string, env map[string]string, profiles []string, dc *docker.Client) CheckGroup {
	active := make(map[string]bool, len(profiles))
	for _, p := range profiles {
		active[p] = true
	}

	var results []CheckResult
	seen := map[string]bool{}
	for _, f := range files {
		for _, b := range composeBindings(f, env, active) {
			if seen[b.label()] {
				continue // a port claimed twice is the catalog's problem, not ours
			}
			seen[b.label()] = true
			if r, collides := checkBinding(b, dc); collides {
				results = append(results, r)
			}
		}
	}

	if len(results) == 0 {
		results = append(results, CheckResult{
			Name: "Published ports", Status: StatusPass,
			Message: "No host port collisions",
		})
	}
	return CheckGroup{Title: "Ports", Results: results}
}

// checkBinding returns a result only when b is a genuine collision: the port is
// taken and the container that wants it is not the one holding it.
func checkBinding(b portBinding, dc *docker.Client) (CheckResult, bool) {
	if portFree(b) {
		return CheckResult{}, false
	}
	if dc == nil {
		return CheckResult{
			Name: b.container, Status: StatusWarn,
			Message: fmt.Sprintf("host port %s is in use; cannot tell whether %s holds it (Docker unavailable)",
				b.label(), b.container),
		}, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	state := dc.ContainerState(ctx, b.container)
	cancel()
	if state == "running" {
		return CheckResult{}, false // bound by the container that should bind it
	}
	return CheckResult{
		Name: b.container, Status: StatusFail,
		Message: fmt.Sprintf("host port %s is held by another process — %s (%s) will fail to start; "+
			"free the port or override it in config.yaml",
			b.label(), b.container, filepath.Base(filepath.Dir(b.origin))),
	}, true
}

// portFree reports whether b's host port can still be bound. Docker binds the
// same way, so a refused bind here is a refused bind there — including the
// asymmetric cases, where 0.0.0.0 and 127.0.0.1 on one port conflict.
func portFree(b portBinding) bool {
	addr := net.JoinHostPort(b.hostIP, b.hostPort)
	if b.proto == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// composeBindings extracts the published host ports from one compose file.
func composeBindings(file string, env map[string]string, active map[string]bool) []portBinding {
	data, err := os.ReadFile(file) // #nosec G304 -- file is core/ or services/*/ under the config dir
	if err != nil {
		return nil
	}
	var cf struct {
		Services map[string]struct {
			ContainerName string   `yaml:"container_name"`
			Profiles      []string `yaml:"profiles"`
			// Long-form entries ({target: 80, published: 8080}) parse into the
			// `any` and are skipped — nothing in this stack uses them.
			Ports []any `yaml:"ports"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal(data, &cf) != nil {
		return nil
	}

	names := make([]string, 0, len(cf.Services))
	for name := range cf.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []portBinding
	for _, name := range names {
		svc := cf.Services[name]
		if !profileActive(svc.Profiles, active) {
			continue
		}
		container := svc.ContainerName
		if container == "" {
			container = name
		}
		for _, raw := range svc.Ports {
			s, ok := raw.(string)
			if !ok {
				continue
			}
			if b, ok := parsePortMapping(expandVars(s, env)); ok {
				b.container, b.origin = container, file
				out = append(out, b)
			}
		}
	}
	return out
}

// profileActive reports whether a service with these profiles will start.
// No profiles means always; otherwise any one active profile is enough.
func profileActive(svcProfiles []string, active map[string]bool) bool {
	if len(svcProfiles) == 0 {
		return true
	}
	for _, p := range svcProfiles {
		if active[p] {
			return true
		}
	}
	return false
}

// parsePortMapping reads compose short syntax: "port", "host:container",
// "ip:host:container", any with a "/udp" or "/tcp" suffix.
//
// A bare "8080" publishes on a host port Docker picks at random, so there is no
// fixed port to check — same reading the catalog audit uses. Ranges and
// bracketed IPv6 hosts are skipped too: nothing in this stack publishes either,
// and guessing wrong would mean crying collision over a port nobody binds.
func parsePortMapping(s string) (portBinding, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") || strings.Contains(s, "-") {
		return portBinding{}, false
	}

	proto := "tcp"
	if base, p, found := strings.Cut(s, "/"); found {
		if p != "tcp" && p != "udp" {
			return portBinding{}, false
		}
		s, proto = base, p
	}

	parts := strings.Split(s, ":")
	b := portBinding{proto: proto}
	switch len(parts) {
	case 2:
		b.hostPort = parts[0]
	case 3:
		b.hostIP, b.hostPort = parts[0], parts[1]
	default:
		return portBinding{}, false // 1 part = ephemeral host port, nothing to check
	}
	if b.hostPort == "" || strings.ContainsAny(b.hostPort, "${}") {
		return portBinding{}, false // unresolved variable — no port to test
	}
	return b, true
}

// expandVars resolves ${VAR} and ${VAR:-default} against env. Compose does the
// same substitution before it binds anything, so a check that skipped it would
// test the wrong port whenever a default is overridden.
func expandVars(s string, env map[string]string) string {
	var sb strings.Builder
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			sb.WriteString(s)
			return sb.String()
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			sb.WriteString(s)
			return sb.String()
		}
		sb.WriteString(s[:start])
		ref := s[start+2 : start+end]
		s = s[start+end+1:]

		name, def, hasDef := strings.Cut(ref, ":-")
		if v, ok := env[name]; ok && v != "" {
			sb.WriteString(v)
		} else if hasDef {
			sb.WriteString(def)
		}
	}
}
