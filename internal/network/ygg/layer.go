// Package ygg implements the NetworkLayer interface for the Yggdrasil IPv6
// mesh node. Manages yggdrasil container lifecycle and per-service socat
// TCP6→TCP4 port forwarders.
package ygg

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "yggdrasil"

// Layer implements network.NetworkLayer for Yggdrasil mesh.
type Layer struct {
	repoRoot    string
	runner      *run.Commander
	restartHook func() error
}

// New creates a new Yggdrasil layer.
func New(repoRoot string, runner *run.Commander) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner}
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

func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	for _, port := range ports {
		// 1. Write Caddy config to conf.d-ygg/
		caddyBlock := l.caddyBlock(displayName, svcName, port)
		if err := l.writeCaddyConfig(svcName, port.Name, caddyBlock); err != nil {
			return fmt.Errorf("writing caddy config: %w", err)
		}
		// 2. Write socat forwarder
		if err := l.appendForwarder(svcName, port.Name, port.Port); err != nil {
			return fmt.Errorf("writing socat forwarder: %w", err)
		}
	}
	// 3. Restart yggdrasil to pick up new forwarders
	return l.restart()
}

func (l *Layer) Disable(svcName string) error {
	// Remove Caddy config
	caddyDir := l.CaddyConfigDir(l.repoRoot)
	_ = os.Remove(filepath.Join(caddyDir, svcName+".conf"))

	// Remove socat forwarder
	_ = l.removeForwarder(svcName)

	return l.restart()
}

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-ygg")
}

func (l *Layer) caddyBlock(displayName, svcName string, port network.PortSelection) string {
	return fmt.Sprintf("%s.ygg {\n    reverse_proxy %s:%d\n}\n", displayName, svcName, port.Port)
}

func (l *Layer) writeCaddyConfig(svcName, portName, content string) error {
	dir := l.CaddyConfigDir(l.repoRoot)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating caddy config dir: %w", err)
	}
	path := filepath.Join(dir, svcName+".conf")
	return os.WriteFile(path, []byte(content), 0o600)
}

// ── Ygg-specific helpers ─────────────────────────────────────────────────────

func (l *Layer) socatDir() string {
	return filepath.Join(l.repoRoot, "yggdrasil", "socat.d")
}

func (l *Layer) appendForwarder(name, portName string, port int) error {
	socatDir := l.socatDir()
	if err := os.MkdirAll(socatDir, 0o750); err != nil {
		return fmt.Errorf("creating socat.d: %w", err)
	}
	fwdPath := filepath.Join(socatDir, name+".forward")
	content := fmt.Sprintf("PORT=%d\nTARGET=%s:%d\n", port, name, port)
	return os.WriteFile(fwdPath, []byte(content), 0o600)
}

func (l *Layer) removeForwarder(name string) error {
	fwdPath := filepath.Join(l.socatDir(), name+".forward")
	err := os.Remove(fwdPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (l *Layer) restart() error {
	if l.restartHook != nil {
		return l.restartHook()
	}
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot), l.env(),
		"restart", containerName)
}

func (l *Layer) env() map[string]string {
	return map[string]string{}
}
