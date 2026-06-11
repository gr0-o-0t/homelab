// Package tor implements the NetworkLayer interface for Tor onion service proxy.
// Manages tor container lifecycle and per-service .onion hidden service configs
// via torrc.d/ configuration files and SIGHUP reloads.
package tor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const (
	containerName       = "tor"
	torHiddenServiceDir = "/var/lib/tor/hidden_service"
)

// Layer implements network.NetworkLayer for Tor onion services.
type Layer struct {
	repoRoot string
	runner   *run.Commander

	// reloadHook, if non-nil, replaces reload() for testing.
	reloadHook func() error
}

// New creates a new Tor layer.
func New(repoRoot string, runner *run.Commander) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner}
}

// newForTest creates a Tor layer with an injected reload hook for testing.
func newForTest(repoRoot string, hook func() error) *Layer {
	return &Layer{repoRoot: repoRoot, runner: run.Default(), reloadHook: hook}
}

// compile-time check
var _ network.NetworkLayer = (*Layer)(nil)

// ── Identity ──────────────────────────────────────────────────────────────────

func (l *Layer) Name() string          { return "tor" }
func (l *Layer) Label() string         { return "Tor onion service proxy" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "tor" }

// ── Lifecycle ─────────────────────────────────────────────────────────────────

func (l *Layer) Start() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot),
		l.env(),
		"--profile", "tor", "up", "-d",
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

// Enable writes Caddy config to conf.d-tor/ AND torrc.d config for the service,
// then reloads tor. The Caddy block routes <displayName>.onion → svcName:<port>.
func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	for _, port := range ports {
		// 1. Write Caddy config to conf.d-tor/
		caddyBlock := l.caddyBlock(displayName, svcName, port)
		if err := l.writeCaddyConfig(svcName, port.Name, caddyBlock); err != nil {
			return fmt.Errorf("writing caddy config: %w", err)
		}

		// 2. Write torrc.d config
		if err := l.appendTorService(svcName, port.Port); err != nil {
			return fmt.Errorf("writing torrc config: %w", err)
		}
	}

	// 3. Reload tor to pick up new configs
	return l.reload()
}

// Disable removes both Caddy config and torrc.d config for the service, then reloads.
func (l *Layer) Disable(svcName string) error {
	// Remove Caddy config
	caddyDir := l.CaddyConfigDir(l.repoRoot)
	caddyPath := filepath.Join(caddyDir, svcName+".conf")
	if err := os.Remove(caddyPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing caddy config: %w", err)
	}

	// Remove torrc.d config
	confPath := l.torServicePath(svcName)
	if err := os.Remove(confPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing torrc config: %w", err)
	}

	return l.reload()
}

// ── Config ────────────────────────────────────────────────────────────────────

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-tor")
}

// ── Caddy block generation ────────────────────────────────────────────────────

func (l *Layer) caddyBlock(displayName, svcName string, port network.PortSelection) string {
	return fmt.Sprintf("%s.onion {\n    reverse_proxy %s:%d\n}\n", displayName, svcName, port.Port)
}

func (l *Layer) writeCaddyConfig(svcName, portName, content string) error {
	dir := l.CaddyConfigDir(l.repoRoot)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating caddy config dir: %w", err)
	}

	// For tor, use just the service name as filename (no port suffix)
	path := filepath.Join(dir, svcName+".conf")
	return os.WriteFile(path, []byte(content), 0o600)
}

// ── Tor-specific helpers ─────────────────────────────────────────────────────

func (l *Layer) torServicePath(name string) string {
	return filepath.Join(l.repoRoot, "tor", "torrc.d", name+".conf")
}

func (l *Layer) appendTorService(name string, port int) error {
	confDir := filepath.Join(l.repoRoot, "tor", "torrc.d")
	if err := os.MkdirAll(confDir, 0o750); err != nil {
		return fmt.Errorf("creating torrc.d: %w", err)
	}
	// Pre-create hidden service directory so Docker doesn't create it as
	// root:root, which would prevent the non-root tor user inside the
	// container from writing onion keys.
	hsDir := filepath.Join(l.repoRoot, "tor", "hidden_service", name)
	if err := os.MkdirAll(hsDir, 0777); err != nil {
		return fmt.Errorf("creating hidden service dir: %w", err)
	}
	confPath := l.torServicePath(name)
	content := fmt.Sprintf("HiddenServiceDir %s/%s\nHiddenServicePort 80 %s:%d\n",
		torHiddenServiceDir, name, name, port)
	return os.WriteFile(confPath, []byte(content), 0o600)
}

func (l *Layer) reload() error {
	if l.reloadHook != nil {
		return l.reloadHook()
	}
	return l.runner.DockerExec(containerName,
		"sh", "-c", "kill -HUP $(pidof tor)")
}

// ── env ───────────────────────────────────────────────────────────────────────

func (l *Layer) env() map[string]string {
	// In Slice 6, this will use buildEnv from cmd/. For now, minimal env.
	return map[string]string{}
}
