// Package i2p implements the NetworkLayer interface for the i2pd router
// and eepsite tunnel management. Manages i2pd container lifecycle and
// per-service eepsite tunnels via tunnels.conf INI files.
package i2p

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "i2p"

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
	reloadHook func() error
}

// New creates a new I2P layer.
func New(repoRoot string, runner *run.Commander) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner}
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
		if err := l.appendTunnel(svcName, port.Port); err != nil {
			return fmt.Errorf("writing i2p tunnel: %w", err)
		}
	}
	return l.reload()
}

// Disable removes the I2P tunnel config for the service and reloads. Caddy
// config removal is handled separately by internal/configgen.
func (l *Layer) Disable(svcName string) error {
	_ = l.removeTunnel(svcName)
	return l.reload()
}

// ── Config ────────────────────────────────────────────────────────────────────

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-i2p")
}

// ── I2P-specific helpers ─────────────────────────────────────────────────────

func (l *Layer) tunnelsPath() string {
	return filepath.Join(l.repoRoot, "i2p", "tunnels.conf")
}

// appendTunnel appends an HTTP tunnel section to tunnels.conf.
// The tunnel routes .i2p traffic through caddy:80 with hostoverride
// so Caddy can route by Host header.
func (l *Layer) appendTunnel(name string, port int) error {
	tunPath := l.tunnelsPath()

	// Ensure parent directory exists (like Tor's appendTorService creates torrc.d/)
	if err := os.MkdirAll(filepath.Dir(tunPath), 0o750); err != nil {
		return fmt.Errorf("creating i2p config dir: %w", err)
	}

	existing, err := l.parseTunnels()
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading tunnels.conf: %w", err)
	}
	for _, t := range existing {
		if t.Name == name {
			return fmt.Errorf("tunnel for %q already exists in tunnels.conf", name)
		}
	}

	f, err := os.OpenFile(tunPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening tunnels.conf: %w", err)
	}
	defer func() { _ = f.Close() }()

	section := fmt.Sprintf("\n[%s]\ntype = http\nhost = caddy\nport = 80\nhostoverride = %s.i2p\nkeys = %s.dat\n",
		name, name, name)
	if _, err := f.WriteString(section); err != nil {
		return fmt.Errorf("writing tunnels.conf: %w", err)
	}
	return nil
}

// removeTunnel removes a named tunnel section from tunnels.conf.
func (l *Layer) removeTunnel(name string) error {
	tunPath := l.tunnelsPath()

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

// parseTunnels reads and parses tunnels.conf into sections.
func (l *Layer) parseTunnels() ([]TunnelSection, error) {
	path := l.tunnelsPath()
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

func (l *Layer) reload() error {
	if l.reloadHook != nil {
		return l.reloadHook()
	}
	return l.runner.DockerExec(containerName, "kill", "-HUP", "1")
}

func (l *Layer) env() map[string]string {
	return map[string]string{}
}
