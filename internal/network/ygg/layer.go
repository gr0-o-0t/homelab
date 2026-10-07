// Package ygg implements the NetworkLayer interface for the Yggdrasil IPv6
// mesh node. Manages yggdrasil container lifecycle and per-service socat
// TCP6→TCP4 port forwarders.
package ygg

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "yggdrasil"

// Layer implements network.NetworkLayer for Yggdrasil mesh.
type Layer struct {
	repoRoot   string
	runner     *run.Commander
	envFn      network.EnvFunc
	reloadHook func() error

	// addrCache memoizes the node address; see network.AddressCache.
	addrCache network.AddressCache
}

// New creates a new Yggdrasil layer.
func New(repoRoot string, runner *run.Commander, envFn network.EnvFunc) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner, envFn: envFn}
}

func newForTest(repoRoot string, hook func() error) *Layer {
	return &Layer{repoRoot: repoRoot, runner: run.Default(), reloadHook: hook}
}

var (
	_ network.NetworkLayer = (*Layer)(nil)
	_ network.Configurer   = (*Layer)(nil)
)

func (l *Layer) Name() string          { return "ygg" }
func (l *Layer) Label() string         { return "Yggdrasil mesh node" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "yggdrasil" }
func (l *Layer) Flag() string          { return "ygg" }
func (l *Layer) LegacyConfDir() string { return "conf.d-ygg" }

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

// Configure gives each of the service's ports a mesh port and a socat
// forwarder pointing at Caddy (tailscale:<port>), records the allocation in
// the service's exposure.yaml, then has yggdrasil reconcile its forwarders.
func (l *Layer) Configure(svcName, _ string, ports []network.PortSelection) error {
	alloc, err := l.allocate(svcName, ports)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, p := range ports {
		file := configgen.PortFileName(svcName, p.Name)
		keep[file] = true
		if err := l.writeForwarder(file, alloc[exposure.MeshKey(p.Name)]); err != nil {
			return fmt.Errorf("writing socat forwarder: %w", err)
		}
	}
	// A narrower --ports selection drops the forwarders of deselected ports.
	var stale []string
	for _, f := range l.fileNames(svcName) {
		if !keep[f] {
			stale = append(stale, f)
		}
	}
	if err := removeExact(l.socatDir(), stale, ".forward"); err != nil {
		return err
	}
	if err := exposure.Update(l.repoRoot, svcName, func(s *exposure.State) { s.YggPorts = alloc }); err != nil {
		return err
	}
	return l.reload()
}

// Teardown removes the service's socat forwarders, releases its mesh ports
// and has yggdrasil reconcile.
func (l *Layer) Teardown(svcName string) error {
	_ = l.removeForwarder(svcName)
	if err := exposure.Update(l.repoRoot, svcName, func(s *exposure.State) { s.YggPorts = nil }); err != nil {
		return err
	}
	return l.reload()
}

// Sites returns one `:<mesh port>` site per port. The mesh has no naming, so
// clients reach a service at [<node address>]:<port> and Caddy routes by
// listening port, not Host header. A port-only site address serves plain HTTP:
// there is no hostname for automatic HTTPS to get a certificate for — which is
// what we want, since yggdrasil already encrypts the transport.
//
// The mesh ports are the ones Configure recorded in exposure.yaml.
func (l *Layer) Sites(svcName, _ string, ports []network.PortSelection) ([]network.Site, error) {
	st, err := exposure.Load(l.repoRoot, svcName)
	if err != nil {
		return nil, err
	}
	sites := make([]network.Site, 0, len(ports))
	for _, p := range ports {
		meshPort := st.YggPorts[exposure.MeshKey(p.Name)]
		if meshPort == 0 {
			// No forwarder carries this port onto the mesh — a port declared
			// since the service was enabled, or one an older install never
			// forwarded — so a block for it would route nothing. `homelab
			// enable <svc> --ygg` allocates it.
			continue
		}
		sites = append(sites, network.Site{
			PortName: p.Name,
			Address:  fmt.Sprintf(":%d", meshPort),
			Port:     p.Port,
			Comment:  fmt.Sprintf("# Yggdrasil: %s → reachable at http://[<node address>]:%d\n", svcName, meshPort),
		})
	}
	return sites, nil
}

// ServiceAddresses pairs the node's mesh address with each port allocated to
// this service. The mesh has no naming, so both halves are required and
// neither can be templated from the service name.
func (l *Layer) ServiceAddresses(svcName string, _ map[string]string) []network.ServiceAddress {
	st, err := exposure.Load(l.repoRoot, svcName)
	if err != nil || len(st.YggPorts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(st.YggPorts))
	for k := range st.YggPorts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	addr := l.NodeAddress()
	out := make([]network.ServiceAddress, 0, len(keys))
	for _, k := range keys {
		a := network.ServiceAddress{URL: ServiceURL(addr, st.YggPorts[k])}
		if addr == "" {
			a.Note = "node address unknown — is the yggdrasil container running?"
		}
		out = append(out, a)
	}
	return out
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

// writeForwarder writes one port's socat forwarder.
//
// The forwarder targets tailscale:<meshPort>, not the service container:
// Caddy runs in the tailscale container's network namespace, so that is the
// address everything off-namespace uses to reach it (cloudflared and i2pd do
// the same). Going straight to the service would bypass Caddy entirely, which
// is what the generated `<name>.ygg` Caddy blocks used to pretend wasn't
// happening.
func (l *Layer) writeForwarder(file string, meshPort int) error {
	if err := os.MkdirAll(l.socatDir(), 0o750); err != nil {
		return fmt.Errorf("creating socat.d: %w", err)
	}
	content := fmt.Sprintf("PORT=%d\nTARGET=tailscale:%d\n", meshPort, meshPort)
	return os.WriteFile(filepath.Join(l.socatDir(), file+".forward"), []byte(content), 0o600)
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

// allocate picks the mesh port of each port: the one it already has in
// exposure.yaml if it has one — re-enabling must not move a service, since
// peers reach it as [addr]:port and there is no name to re-resolve — otherwise
// the lowest port at or above meshPortBase that no service holds. Every
// service's exposure.yaml is the allocation registry.
//
// Two services on 8080 used to mean two socats binding 8080, the second dying
// with EADDRINUSE in a log nobody reads.
func (l *Layer) allocate(svcName string, ports []network.PortSelection) (map[string]int, error) {
	st, err := exposure.Load(l.repoRoot, svcName)
	if err != nil {
		return nil, err
	}
	taken, err := exposure.MeshPorts(l.repoRoot, svcName)
	if err != nil {
		return nil, err
	}
	alloc := map[string]int{}
	for _, p := range ports { // keep existing allocations first, so a new port cannot take one
		key := exposure.MeshKey(p.Name)
		if mp := st.YggPorts[key]; mp != 0 && !taken[mp] {
			alloc[key] = mp
			taken[mp] = true
		}
	}
	next := meshPortBase
	for _, p := range ports {
		key := exposure.MeshKey(p.Name)
		if alloc[key] != 0 {
			continue
		}
		for next <= 65535 && taken[next] {
			next++
		}
		if next > 65535 {
			return nil, fmt.Errorf("no free mesh port at or above %d", meshPortBase)
		}
		alloc[key] = next
		taken[next] = true
	}
	return alloc, nil
}

// removeForwarder removes every forward file for the service — both the
// default-name one and any per-port ones — since Disable isn't told which
// ports were previously enabled.
func (l *Layer) removeForwarder(name string) error {
	return removeExact(l.socatDir(), l.fileNames(name), ".forward")
}

// fileNames lists every basename this service's forwarders can have: the default one plus one per declared port (see configgen.PortFileName).
//
// Exact names, not a `<name>-*` glob: the glob for "foo" also matches the files
// of a service called "foo-bar", so disabling one tore down the other's mesh
// exposure.
func (l *Layer) fileNames(name string) []string {
	names := []string{name}
	info, err := configgen.LoadServiceInfo(l.repoRoot, name)
	if err != nil {
		return names
	}
	for portName := range info.Ports {
		if n := configgen.PortFileName(name, portName); n != name {
			names = append(names, n)
		}
	}
	return names
}

// removeExact removes dir/<name><ext> for each name, returning the first error
// that isn't "already gone".
func removeExact(dir string, names []string, ext string) error {
	var firstErr error
	for _, n := range names {
		if err := os.Remove(filepath.Join(dir, n+ext)); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// reload makes the running node pick up socat.d. SIGHUP, not a restart: the
// entrypoint reconciles forwarders on HUP (starts new ones, stops removed
// ones) without touching the yggdrasil daemon. A restart dropped every mesh
// peering, and peers take minutes to come back.
func (l *Layer) reload() error {
	if l.reloadHook != nil {
		return l.reloadHook()
	}
	// A stopped node has nothing to reload; it reads socat.d on start.
	if l.runner.ContainerStatus(containerName) != "running" {
		return nil
	}
	// RunTo(io.Discard): docker kill echoes the container name on stdout.
	return l.runner.RunTo(io.Discard, "docker", "kill", "-s", "HUP", containerName)
}

func (l *Layer) env() map[string]string {
	if l.envFn == nil {
		return map[string]string{}
	}
	return l.envFn()
}
