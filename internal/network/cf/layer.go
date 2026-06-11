// Package cf implements the NetworkLayer interface for Cloudflare Tunnel
// (cloudflared). Manages cloudflared container lifecycle and public DNS
// route management for exposing services via Cloudflare's edge network.
package cf

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "cloudflared"

// Layer implements network.NetworkLayer for Cloudflare Tunnel.
type Layer struct {
	repoRoot   string
	runner     *run.Commander
	reloadHook func() error
}

// New creates a new Cloudflare layer.
func New(repoRoot string, runner *run.Commander) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner}
}

func newForTest(repoRoot string, hook func() error) *Layer {
	return &Layer{repoRoot: repoRoot, runner: run.Default(), reloadHook: hook}
}

var _ network.NetworkLayer = (*Layer)(nil)

func (l *Layer) Name() string          { return "cf" }
func (l *Layer) Label() string         { return "Cloudflare Tunnel" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "tunnel" }

func (l *Layer) Start() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot), l.env(),
		"--profile", "tunnel", "up", "-d")
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

func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	for _, port := range ports {
		caddyBlock := l.caddyBlock(displayName, svcName, port)
		if err := l.writeCaddyConfig(svcName, port.Name, caddyBlock); err != nil {
			return fmt.Errorf("writing caddy config: %w", err)
		}
	}
	return nil
}

func (l *Layer) Disable(svcName string) error {
	caddyDir := l.CaddyConfigDir(l.repoRoot)
	_ = os.Remove(filepath.Join(caddyDir, svcName+".conf"))
	return nil
}

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-cf")
}

// Caddy block for CF uses http:// prefix (TLS terminated at Cloudflare edge).
func (l *Layer) caddyBlock(displayName, svcName string, port network.PortSelection) string {
	domain := fmt.Sprintf("%s.{$DOMAIN}", displayName)
	return fmt.Sprintf("http://%s {\n    reverse_proxy %s:%d\n}\n", domain, svcName, port.Port)
}

func (l *Layer) writeCaddyConfig(svcName, portName, content string) error {
	dir := l.CaddyConfigDir(l.repoRoot)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating caddy config dir: %w", err)
	}
	path := filepath.Join(dir, svcName+".conf")
	return os.WriteFile(path, []byte(content), 0o600)
}

func (l *Layer) env() map[string]string {
	return map[string]string{}
}
