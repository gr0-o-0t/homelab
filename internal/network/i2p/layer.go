// Package i2p implements the NetworkLayer interface for the i2pd router
// and eepsite tunnel management. Manages i2pd container lifecycle and
// per-service eepsite tunnels via tunnels.conf INI files.
package i2p

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const (
	containerName = "i2p"

	// dataDir is i2pd's DATA_DIR inside the container, where tunnel keys live.
	// Set by the upstream image; our wrapper Dockerfile matches it.
	dataDir = "/home/i2pd/data"
)

// TunnelSection is a parsed eepsite tunnel from tunnels.conf.
type TunnelSection struct {
	Name         string
	Host         string
	Port         string
	Keys         string
	HostOverride string
}

// Layer implements network.NetworkLayer for I2P eepsite tunnels.
type Layer struct {
	repoRoot   string
	runner     *run.Commander
	envFn      network.EnvFunc
	reloadHook func() error

	// b32Cache memoizes per-tunnel b32 lookups; see network.AddressCache.
	cacheMu  sync.Mutex
	b32Cache map[string]*network.AddressCache
}

// New creates a new I2P layer.
func New(repoRoot string, runner *run.Commander, envFn network.EnvFunc) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner, envFn: envFn}
}

// newForTest creates an I2P layer with injected reload hook for testing.
func newForTest(repoRoot string, hook func() error) *Layer {
	return &Layer{repoRoot: repoRoot, runner: run.Default(), reloadHook: hook}
}

// compile-time check
var _ network.NetworkLayer = (*Layer)(nil)

// ── Identity ──────────────────────────────────────────────────────────────────

func (l *Layer) Name() string          { return "i2p" }
func (l *Layer) Label() string         { return "I2P router + eepsite proxy" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "i2p" }

// ── Lifecycle ─────────────────────────────────────────────────────────────────

func (l *Layer) Start() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot),
		l.env(),
		"--profile", "i2p", "up", "-d",
	)
}

func (l *Layer) Stop() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot),
		l.env(),
		"stop", containerName,
	)
}

func (l *Layer) Status() network.Status {
	state := l.runner.ContainerStatus(containerName)
	return network.Status{ContainerState: state}
}

// ── Service exposure ─────────────────────────────────────────────────────────

// Enable appends an I2P tunnel section to tunnels.conf for the service, then
// reloads i2pd. Caddy config (host-override routing to Caddy) is written
// separately by internal/configgen — see cmd/enable.go.
func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	for _, port := range ports {
		if err := l.AppendTunnel(svcName, port.Port); err != nil {
			return fmt.Errorf("writing i2p tunnel: %w", err)
		}
	}
	return l.Reload()
}

// Disable removes the I2P tunnel config for the service and reloads. Caddy
// config removal is handled separately by internal/configgen.
func (l *Layer) Disable(svcName string) error {
	_ = l.RemoveTunnel(svcName)
	return l.Reload()
}

// ── Config ────────────────────────────────────────────────────────────────────

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-i2p")
}

// ── Addressing ────────────────────────────────────────────────────────────────

// ServiceAddresses returns the eepsite's b32 destination, plus the host it
// answers on.
//
// The b32 is the address: it is derived from the tunnel's key and any I2P
// client can open it unassisted. The <service>.<home>.i2p host is only the
// value i2pd puts in the Host header (hostoverride) so Caddy can vhost.
// Nothing about hosting an eepsite publishes a name — listing that host as
// *the* address, as this code used to, sent a browser to a stranger's site.
func (l *Layer) ServiceAddresses(svcName string, _ map[string]string) []network.ServiceAddress {
	var addrs []network.ServiceAddress
	if b32 := l.B32Address(svcName); b32 != "" {
		addrs = append(addrs, network.ServiceAddress{URL: "http://" + b32})
	}
	addrs = append(addrs, network.ServiceAddress{
		URL:  "http://" + l.hostFor(svcName),
		Note: "name, not an address — register it with the addresshelper URL from `homelab i2p list`",
	})
	return addrs
}

// B32Address returns the tunnel's <52 chars>.b32.i2p destination, or "" when
// i2pd has not built the tunnel yet or is not running.
//
// Derived from the key rather than scraped from the web console: the b32 is by
// definition base32(sha256(destination)), so reading the key gives the exact
// answer with no dependency on console markup or on the console being enabled.
func (l *Layer) B32Address(svcName string) string {
	return l.cacheFor(svcName).Get(func() string {
		dest, err := l.destination(svcName)
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(dest)
		return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).
			EncodeToString(sum[:])) + ".b32.i2p"
	})
}

// Base64Destination returns the tunnel's full destination in I2P's base64
// alphabet — the form an addressbook entry stores and a jump link carries.
func (l *Layer) Base64Destination(svcName string) string {
	dest, err := l.destination(svcName)
	if err != nil {
		return ""
	}
	return i2pBase64.EncodeToString(dest)
}

// AddressHelperURL is a one-click registration link for the eepsite's name.
//
// Opened through the router's HTTP proxy, it makes i2pd store
// <host> → <destination> in its addressbook, after which the plain name works
// in that browser. This is I2P's standard jump mechanism and the only way a
// name resolves for anybody.
func (l *Layer) AddressHelperURL(svcName string) string {
	b64 := l.Base64Destination(svcName)
	if b64 == "" {
		return ""
	}
	return fmt.Sprintf("http://%s/?i2paddresshelper=%s", l.hostFor(svcName), b64)
}

// i2pBase64 is base64 with I2P's alphabet: '+' and '/' become '-' and '~'.
var i2pBase64 = base64.NewEncoding(
	"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-~")

// destination reads the tunnel's key file and returns just the Destination
// prefix, which every address is derived from.
//
// The layout is fixed by I2P's common-structures spec: 384 bytes of keys, then
// a certificate whose 2-byte length sits at offsets 385-386. The spec is
// explicit that the total is "387 bytes plus the certificate length ... which
// may be non-zero", so it is read rather than assumed. Everything after that
// is private key material we never touch.
func (l *Layer) destination(svcName string) ([]byte, error) {
	if l.runner == nil {
		return nil, fmt.Errorf("no runner configured")
	}
	data, err := l.runner.Output("docker", "exec", containerName,
		"cat", dataDir+"/"+svcName+".dat")
	if err != nil {
		return nil, fmt.Errorf("reading %s.dat: %w", svcName, err)
	}
	return parseDestination(data, svcName)
}

func parseDestination(data []byte, svcName string) ([]byte, error) {
	const keysLen = 384 // public key + padding + signing key
	if len(data) < keysLen+3 {
		return nil, fmt.Errorf("%s.dat is too short to hold a destination", svcName)
	}
	total := keysLen + 3 + int(binary.BigEndian.Uint16(data[keysLen+1:keysLen+3]))
	if len(data) < total {
		return nil, fmt.Errorf("%s.dat truncated: destination needs %d bytes, file has %d",
			svcName, total, len(data))
	}
	return data[:total], nil
}

// cacheFor returns this tunnel's address cache, creating it on first use.
func (l *Layer) cacheFor(svcName string) *network.AddressCache {
	l.cacheMu.Lock()
	defer l.cacheMu.Unlock()
	if l.b32Cache == nil {
		l.b32Cache = map[string]*network.AddressCache{}
	}
	if l.b32Cache[svcName] == nil {
		l.b32Cache[svcName] = &network.AddressCache{}
	}
	return l.b32Cache[svcName]
}

// ── I2P-specific helpers ─────────────────────────────────────────────────────
//
// Exported so cmd/i2p.go (the standalone `homelab i2p enable/disable/list`
// commands) can call these directly instead of maintaining its own copy —
// two copies previously diverged (one was missing the tunnels.conf
// directory creation on first use, and they disagreed on whether removing a
// missing tunnel is an error), which broke the very workflow i2pEnableCmd's
// own help text suggested (enable via `i2p enable`, then via `enable --i2p`).

// TunnelsPath returns the path to i2pd's tunnels.conf.
func (l *Layer) TunnelsPath() string {
	return filepath.Join(l.repoRoot, "i2p", "tunnels.conf")
}

// AppendTunnel appends an HTTP tunnel section to tunnels.conf, routing
// <name>.i2p traffic through tailscale:80 with hostoverride so Caddy can route
// by Host header. Idempotent: a tunnel with the same name already present
// is left as-is rather than erroring, since enabling i2p for a service twice
// (e.g. once via `homelab i2p enable`, once via `homelab enable --i2p`) is a
// normal, expected sequence, not a conflict.
func (l *Layer) AppendTunnel(name string, port int) error {
	tunPath := l.TunnelsPath()

	if err := os.MkdirAll(filepath.Dir(tunPath), 0o750); err != nil {
		return fmt.Errorf("creating i2p config dir: %w", err)
	}

	existing, err := l.ParseTunnels()
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading tunnels.conf: %w", err)
	}
	for _, t := range existing {
		if t.Name != name {
			continue
		}
		if t.HostOverride == l.hostFor(name) {
			return nil // already configured
		}
		// The host changed — a renamed service, or the home subdomain moved.
		// Leaving the old section would keep i2pd stamping a Host header that
		// no longer matches any Caddy site block, i.e. a silent 404 on an
		// eepsite that looks configured.
		if err := l.RemoveTunnel(name); err != nil {
			return fmt.Errorf("replacing stale tunnel: %w", err)
		}
		break
	}

	f, err := os.OpenFile(tunPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening tunnels.conf: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Upstream is "tailscale", not "caddy": Caddy runs with
	// network_mode: service:tailscale, so it has no identity of its own on the
	// home-services network and Docker DNS has no "caddy" record. Everything
	// off-namespace reaches Caddy through the tailscale container (cloudflared
	// does the same). hostoverride sets the Host: header i2pd forwards, which
	// is what Caddy's site block matches on — so it must be byte-identical to
	// what configgen generates, which is why both call I2PHost.
	section := fmt.Sprintf("\n[%s]\ntype = http\nhost = tailscale\nport = 80\nhostoverride = %s\nkeys = %s.dat\n",
		name, l.hostFor(name), name)
	if _, err := f.WriteString(section); err != nil {
		return fmt.Errorf("writing tunnels.conf: %w", err)
	}
	return nil
}

// hostFor is the Host header this tunnel stamps: <service>.<home>.i2p, with
// the home subdomain resolved — i2pd does no environment expansion.
func (l *Layer) hostFor(name string) string {
	return configgen.I2PHost(name, l.env()["HOME_SUBDOMAIN"])
}

// RemoveTunnel removes a named tunnel section from tunnels.conf.
// Idempotent: a missing tunnel (or missing tunnels.conf) is not an error.
func (l *Layer) RemoveTunnel(name string) error {
	tunPath := l.TunnelsPath()

	data, err := os.ReadFile(tunPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading tunnels.conf: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	start, end, found := sectionRange(lines, name)
	if !found {
		return nil // already removed
	}

	// Remove the section including preceding blank line
	removeStart := start
	if removeStart > 0 && strings.TrimSpace(lines[removeStart-1]) == "" {
		removeStart--
	}
	var newLines []string
	newLines = append(newLines, lines[:removeStart]...)
	newLines = append(newLines, lines[end:]...)
	return os.WriteFile(tunPath, []byte(strings.Join(newLines, "\n")), 0o600)
}

// ParseTunnels reads and parses tunnels.conf into sections.
func (l *Layer) ParseTunnels() ([]TunnelSection, error) {
	path := l.TunnelsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var tunnels []TunnelSection
	lines := strings.Split(string(data), "\n")

	reSection := regexp.MustCompile(`^\[(.+)\]$`)
	var current *TunnelSection
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			if line == "" && current != nil && current.Host != "" {
				tunnels = append(tunnels, *current)
				current = nil
			}
			continue
		}

		if m := reSection.FindStringSubmatch(line); m != nil {
			if current != nil && current.Host != "" {
				tunnels = append(tunnels, *current)
			}
			current = &TunnelSection{Name: m[1]}
			continue
		}

		if current == nil {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "host":
			current.Host = val
		case "port":
			current.Port = val
		case "keys":
			current.Keys = val
		case "hostoverride":
			current.HostOverride = val
		}
	}

	if current != nil && current.Host != "" {
		tunnels = append(tunnels, *current)
	}

	return tunnels, nil
}

// sectionRange returns the start and end line indices (0-based, end-exclusive)
// of the section with the given name in the parsed lines.
func sectionRange(lines []string, name string) (int, int, bool) {
	re := regexp.MustCompile(`^\[` + regexp.QuoteMeta(name) + `\]$`)
	start := -1
	for i, line := range lines {
		if re.MatchString(strings.TrimSpace(line)) {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, false
	}

	end := start + 1
	for end < len(lines) {
		trimmed := strings.TrimSpace(lines[end])
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") && !strings.HasPrefix(trimmed, "##") {
			break
		}
		end++
	}

	return start, end, true
}

// Reload restarts i2pd so it picks up tunnels.conf.
//
// Not SIGHUP, despite i2pd documenting "HUP — reload tunnels configuration
// files": the container reads $DATA_DIR/tunnels.conf, which docker-entrypoint.i2p.sh
// copies from the read-only /config-host mount *once at startup*. A HUP would
// faithfully re-read that stale copy and report success, which is how tunnel
// changes silently did nothing before. (Upstream HUP handling is also flaky —
// PurpleI2P/i2pd#1532, #1294.)
//
// ponytail: a restart costs the router a few minutes of netdb reintegration.
// If that becomes annoying, point `tunconf` at the live /config-host file in
// i2pd.conf and go back to SIGHUP.
func (l *Layer) Reload() error {
	if l.reloadHook != nil {
		return l.reloadHook()
	}
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot),
		l.env(),
		"--profile", "i2p", "restart", containerName,
	)
}

func (l *Layer) env() map[string]string {
	if l.envFn == nil {
		return map[string]string{}
	}
	return l.envFn()
}
