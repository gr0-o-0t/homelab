// Package ygg implements the NetworkLayer interface for the Yggdrasil IPv6
// mesh node. Manages yggdrasil container lifecycle and per-service socat
// TCP6→TCP4 port forwarders.
package ygg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "yggdrasil"

// Layer implements network.NetworkLayer for Yggdrasil mesh.
type Layer struct {
	repoRoot    string
	runner      *run.Commander
	envFn       network.EnvFunc
	restartHook func() error

	// addrCache memoizes the node address; see network.AddressCache.
	addrCache network.AddressCache
}

// New creates a new Yggdrasil layer.
func New(repoRoot string, runner *run.Commander, envFn network.EnvFunc) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner, envFn: envFn}
}

func newForTest(repoRoot string, hook func() error) *Layer {
	return &Layer{repoRoot: repoRoot, runner: run.Default(), restartHook: hook}
}

var _ network.NetworkLayer = (*Layer)(nil)

func (l *Layer) Name() string          { return "ygg" }
func (l *Layer) Label() string         { return "Yggdrasil mesh node" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "yggdrasil" }

func (l *Layer) Start() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot), l.env(),
		"--profile", "yggdrasil", "up", "-d")
}

func (l *Layer) Stop() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot), l.env(),
		"stop", containerName)
}

func (l *Layer) Status() network.Status {
	state := l.runner.ContainerStatus(containerName)
	return network.Status{ContainerState: state}
}

// Enable exposes the service on the mesh. Unlike the other layers this one
// writes its own Caddy config instead of taking configgen's: the mesh has no
// naming, so clients reach a service at [<node address>]:<port> and Caddy has
// to route by listening port, not by Host header. The port is only known once
// it has been allocated here, so the site block is generated here too.
//
// Per port: allocate a mesh port, point socat at Caddy (tailscale:<port>),
// and write the matching `:<port>` site block.
func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	svcInfo, err := configgen.LoadServiceInfo(l.repoRoot, svcName)
	if err != nil {
		return fmt.Errorf("reading service config: %w", err)
	}

	for _, port := range ports {
		meshPort, err := l.appendForwarder(svcName, port.Name, port.Port)
		if err != nil {
			return fmt.Errorf("writing socat forwarder: %w", err)
		}
		// A routes-driven service splits paths across containers, so its own
		// route body is the upstream definition; everything else is a single
		// reverse_proxy.
		body := svcInfo.Routes
		if body == "" {
			body = fmt.Sprintf("reverse_proxy %s:%d\n", svcName, port.Port)
		}
		if err := l.writeCaddyBlock(svcName, port.Name, meshPort, body); err != nil {
			return fmt.Errorf("writing caddy block: %w", err)
		}
	}
	return l.restart()
}

// Disable removes the socat forwarders and Caddy blocks for the service and
// restarts yggdrasil.
func (l *Layer) Disable(svcName string) error {
	_ = l.removeForwarder(svcName)
	_ = l.removeCaddyBlocks(svcName)
	return l.restart()
}

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-ygg")
}

// ServiceAddresses pairs the node's mesh address with the port allocated to
// this service. The mesh has no naming, so both halves are required and
// neither can be templated from the service name.
func (l *Layer) ServiceAddresses(svcName string, _ map[string]string) []network.ServiceAddress {
	port := l.MeshPort(svcName)
	if port == 0 {
		return nil
	}
	addr := l.NodeAddress()
	a := network.ServiceAddress{URL: ServiceURL(addr, port)}
	if addr == "" {
		a.Note = "node address unknown — is the yggdrasil container running?"
	}
	return []network.ServiceAddress{a}
}

// ── Ygg-specific helpers ─────────────────────────────────────────────────────

// NodeAddress returns the node's mesh IPv6 address, or "" when the node isn't
// running or the admin endpoint doesn't answer. Cosmetic — never fatal.
//
// The response shape is yggdrasil's admin GetSelfResponse, whose address field
// is `address` at the top level.
func (l *Layer) NodeAddress() string {
	return l.addrCache.Get(func() string {
		if l.runner == nil {
			return ""
		}
		out, err := l.runner.Output("docker", "exec", containerName,
			"yggdrasilctl", "-endpoint=tcp://127.0.0.1:9001", "-json", "getSelf")
		if err != nil {
			return ""
		}
		var self struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(out, &self); err != nil {
			return ""
		}
		return self.Address
	})
}

// MeshPort returns the mesh port allocated to a service, or 0 if it has no
// forwarder. socat.d is the allocation registry, so this needs no daemon.
func (l *Layer) MeshPort(svcName string) int {
	taken, err := l.takenPorts()
	if err != nil {
		return 0
	}
	return taken[svcName]
}

// ServiceURL formats the address a mesh peer opens. Either half can be
// missing — the node may be stopped (no address) or the service may not be
// exposed (no port) — so the placeholder names which half is unknown instead
// of implying a working URL.
func ServiceURL(addr string, port int) string {
	if addr == "" {
		addr = "<node address: homelab ygg status>"
	}
	if port == 0 {
		return fmt.Sprintf("http://[%s]", addr)
	}
	return fmt.Sprintf("http://[%s]:%d", addr, port)
}

func (l *Layer) socatDir() string {
	return filepath.Join(l.repoRoot, "yggdrasil", "socat.d")
}

// appendForwarder writes the socat forwarder for one service port and returns
// the mesh port it was given.
//
// The forwarder targets tailscale:<meshPort>, not the service container:
// Caddy runs in the tailscale container's network namespace, so that is the
// address everything off-namespace uses to reach it (cloudflared and i2pd do
// the same). Going straight to the service would bypass Caddy entirely, which
// is what the generated `<name>.ygg` Caddy blocks used to pretend wasn't
// happening.
func (l *Layer) appendForwarder(name, portName string, port int) (int, error) {
	socatDir := l.socatDir()
	if err := os.MkdirAll(socatDir, 0o750); err != nil {
		return 0, fmt.Errorf("creating socat.d: %w", err)
	}

	file := configgen.PortFileName(name, portName)
	meshPort, err := l.allocatePort(file)
	if err != nil {
		return 0, err
	}

	fwdPath := filepath.Join(socatDir, file+".forward")
	content := fmt.Sprintf("PORT=%d\nTARGET=tailscale:%d\n", meshPort, meshPort)
	if err := os.WriteFile(fwdPath, []byte(content), 0o600); err != nil {
		return 0, err
	}
	return meshPort, nil
}

// meshPortBase is where mesh port allocation starts.
//
// Deliberately not the service's own container port: Caddy serves these blocks
// by listening port, in the same instance that serves :80 and :443, so a
// service declaring port 80 would install a `:80 { … }` block — a host-less
// catch-all that outranks nothing and swallows every unmatched request on the
// port the cf/i2p/tor layers share. Allocating out of a private range keeps
// mesh routing off Caddy's own listeners entirely.
const meshPortBase = 9000

// allocatePort picks the mesh port for a forwarder: the one it already has if
// it has one — re-enabling must not move a service, since peers reach it as
// [addr]:port and there is no name to re-resolve — otherwise the lowest free
// port at or above meshPortBase.
//
// Two services on 8080 used to mean two socats binding 8080, the second dying
// with EADDRINUSE in a log nobody reads.
func (l *Layer) allocatePort(file string) (int, error) {
	taken, err := l.takenPorts()
	if err != nil {
		return 0, err
	}
	if p, ok := taken[file]; ok {
		return p, nil
	}

	claimed := make(map[int]bool, len(taken))
	for _, p := range taken {
		claimed[p] = true
	}
	for p := meshPortBase; p <= 65535; p++ {
		if !claimed[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free mesh port at or above %d", meshPortBase)
}

// takenPorts maps forwarder name → mesh port, read from socat.d. The directory
// is the allocation registry; there is no separate state file to drift.
func (l *Layer) takenPorts() (map[string]int, error) {
	matches, err := filepath.Glob(filepath.Join(l.socatDir(), "*.forward"))
	if err != nil {
		return nil, err
	}
	taken := make(map[string]int, len(matches))
	for _, m := range matches {
		data, err := os.ReadFile(m) //nolint:gosec // path from our own glob
		if err != nil {
			continue
		}
		if p := parsePort(string(data)); p > 0 {
			taken[strings.TrimSuffix(filepath.Base(m), ".forward")] = p
		}
	}
	return taken, nil
}

// parsePort reads PORT=<n> out of a .forward file. Returns 0 if absent or
// unparseable — a malformed file just doesn't reserve a port.
func parsePort(content string) int {
	for _, line := range strings.Split(content, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "PORT=")
		if !ok {
			continue
		}
		if p, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
			return p
		}
	}
	return 0
}

// writeCaddyBlock writes the `:<meshPort>` site block for one forwarder. A
// port-only site address serves plain HTTP: there is no hostname, so Caddy's
// automatic HTTPS has nothing to get a certificate for — which is what we
// want, since yggdrasil already encrypts the transport.
func (l *Layer) writeCaddyBlock(svcName, portName string, meshPort int, body string) error {
	dir := l.CaddyConfigDir(l.repoRoot)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating caddy config dir: %w", err)
	}
	header := fmt.Sprintf("# Yggdrasil: %s → reachable at http://[<node address>]:%d\n", svcName, meshPort)
	block := configgen.WrapSiteBlock(fmt.Sprintf(":%d", meshPort), body)
	path := filepath.Join(dir, configgen.PortFileName(svcName, portName)+".conf")
	return os.WriteFile(path, []byte(header+block), 0o600)
}

// removeCaddyBlocks removes every generated site block for the service — the
// default-name one and any per-port ones, mirroring removeForwarder.
func (l *Layer) removeCaddyBlocks(name string) error {
	dir := l.CaddyConfigDir(l.repoRoot)
	return removeMatching(
		filepath.Join(dir, name+".conf"),
		filepath.Join(dir, name+"-*.conf"),
	)
}

// removeForwarder removes every forward file for the service — both the
// default-name one and any per-port ones — since Disable isn't told which
// ports were previously enabled.
func (l *Layer) removeForwarder(name string) error {
	socatDir := l.socatDir()
	return removeMatching(
		filepath.Join(socatDir, name+".forward"),
		filepath.Join(socatDir, name+"-*.forward"),
	)
}

// removeMatching removes every file matching the given glob patterns,
// returning the first error that isn't "already gone".
func removeMatching(patterns ...string) error {
	var firstErr error
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, m := range matches {
			if err := os.Remove(m); err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (l *Layer) restart() error {
	if l.restartHook != nil {
		return l.restartHook()
	}
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot), l.env(),
		"--profile", "yggdrasil", "restart", containerName)
}

func (l *Layer) env() map[string]string {
	if l.envFn == nil {
		return map[string]string{}
	}
	return l.envFn()
}
