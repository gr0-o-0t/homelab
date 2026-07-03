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

// Enable writes a socat forwarder for each port and restarts yggdrasil to
// pick them up. Caddy config is written separately by internal/configgen —
// see cmd/enable.go.
func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	for _, port := range ports {
		if err := l.appendForwarder(svcName, port.Name, port.Port); err != nil {
			return fmt.Errorf("writing socat forwarder: %w", err)
		}
	}
	return l.restart()
}

// Disable removes all socat forwarders for the service and restarts. Caddy
// config removal is handled separately by internal/configgen.
func (l *Layer) Disable(svcName string) error {
	_ = l.removeForwarder(svcName)
	return l.restart()
}

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-ygg")
}

// ── Ygg-specific helpers ─────────────────────────────────────────────────────

func (l *Layer) socatDir() string {
	return filepath.Join(l.repoRoot, "yggdrasil", "socat.d")
}

// forwarderFilename mirrors configgen's per-port naming: a service with
// multiple non-default ports gets one forward file per port instead of each
// port overwriting the last.
func forwarderFilename(name, portName string) string {
	if portName != "" && portName != "default" && portName != "web" {
		return name + "-" + portName
	}
	return name
}

func (l *Layer) appendForwarder(name, portName string, port int) error {
	socatDir := l.socatDir()
	if err := os.MkdirAll(socatDir, 0o750); err != nil {
		return fmt.Errorf("creating socat.d: %w", err)
	}
	fwdPath := filepath.Join(socatDir, forwarderFilename(name, portName)+".forward")
	content := fmt.Sprintf("PORT=%d\nTARGET=%s:%d\n", port, name, port)
	return os.WriteFile(fwdPath, []byte(content), 0o600)
}

// removeForwarder removes every forward file for the service — both the
// default-name one and any per-port ones — since Disable isn't told which
// ports were previously enabled.
func (l *Layer) removeForwarder(name string) error {
	socatDir := l.socatDir()
	patterns := []string{
		filepath.Join(socatDir, name+".forward"),
		filepath.Join(socatDir, name+"-*.forward"),
	}
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
		"restart", containerName)
}

func (l *Layer) env() map[string]string {
	return map[string]string{}
}
