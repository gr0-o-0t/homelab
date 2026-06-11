// Package ipfs implements the NetworkLayer interface for the IPFS Kubo node.
// Manages IPFS container lifecycle and the IPFS Gateway Caddy route
// (gateway.ipfs.<domain> → reverse_proxy to kubo's gateway HTTP port).
package ipfs

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "ipfs"

// Layer implements network.NetworkLayer for the IPFS Kubo node.
type Layer struct {
	repoRoot   string
	runner     *run.Commander
	reloadHook func() error
}

// New creates a new IPFS layer.
func New(repoRoot string, runner *run.Commander) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner}
}

func newForTest(repoRoot string, hook func() error) *Layer {
	return &Layer{repoRoot: repoRoot, runner: run.Default(), reloadHook: hook}
}

var _ network.NetworkLayer = (*Layer)(nil)

func (l *Layer) Name() string          { return "ipfs" }
func (l *Layer) Label() string         { return "IPFS Kubo node" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "ipfs" }

func (l *Layer) Start() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot), l.env(),
		"--profile", "ipfs", "up", "-d")
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

// Enable writes the IPFS Gateway Caddy route. The gateway is a well-known
// service — enable/disable operates on the gateway concept, not per-service.
func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	// IPFS gateway uses a fixed Caddy block, not per-service routing.
	// svcName is the display name for the gateway subdomain.
	caddyBlock := l.caddyBlock(displayName)
	return l.writeCaddyConfig("gateway", caddyBlock)
}

// Disable removes the IPFS Gateway Caddy route.
func (l *Layer) Disable(svcName string) error {
	caddyDir := l.CaddyConfigDir(l.repoRoot)
	_ = os.Remove(filepath.Join(caddyDir, "gateway.conf"))
	return nil
}

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-ipfs")
}

func (l *Layer) caddyBlock(displayName string) string {
	// IPFS Gateway: gateway.$HOME_SUBDOMAIN.$DOMAIN → kubo:8080
	return fmt.Sprintf("gateway.%s {\n    reverse_proxy %s:8080\n}\n", displayName, containerName)
}

func (l *Layer) writeCaddyConfig(name, content string) error {
	dir := l.CaddyConfigDir(l.repoRoot)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating caddy config dir: %w", err)
	}
	path := filepath.Join(dir, name+".conf")
	return os.WriteFile(path, []byte(content), 0o600)
}

func (l *Layer) env() map[string]string {
	return map[string]string{}
}
