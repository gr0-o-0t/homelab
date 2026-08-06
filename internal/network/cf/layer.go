// Package cf implements the NetworkLayer interface for Cloudflare Tunnel
// (cloudflared). Manages cloudflared container lifecycle and public DNS
// route management for exposing services via Cloudflare's edge network.
package cf

import (
	"fmt"
	"path/filepath"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "cloudflared"

// Layer implements network.NetworkLayer for Cloudflare Tunnel.
type Layer struct {
	repoRoot   string
	runner     *run.Commander
	envFn      network.EnvFunc
	reloadHook func() error
}

// New creates a new Cloudflare layer.
func New(repoRoot string, runner *run.Commander, envFn network.EnvFunc) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner, envFn: envFn}
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

// Enable and Disable are no-ops: cf has no extension-native state beyond
// Caddy routing, and Caddy config for every extension (including cf) is now
// written/removed solely by internal/configgen — see cmd/enable.go and
// cmd/disable.go. Kept on the interface for registry/status/logs use.
func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	return nil
}

func (l *Layer) Disable(svcName string) error {
	return nil
}

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-cf")
}

// ServiceAddresses returns the public hostname Cloudflare fronts. Templated,
// not looked up: the tunnel serves whatever DNS name is routed to it, and that
// name is the one this layer wrote into Caddy.
func (l *Layer) ServiceAddresses(svcName string, env map[string]string) []network.ServiceAddress {
	dom := env["DOMAIN"]
	if dom == "" {
		return []network.ServiceAddress{{Note: "DOMAIN not set — run homelab setup"}}
	}
	return []network.ServiceAddress{{URL: fmt.Sprintf("https://%s.%s", svcName, dom)}}
}

func (l *Layer) env() map[string]string {
	if l.envFn == nil {
		return map[string]string{}
	}
	return l.envFn()
}
