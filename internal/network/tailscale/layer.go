// Package tailscale implements the NetworkLayer interface for Tailscale mesh VPN.
// Tailscale is the always-on default extension: it provides the tailnet network
// interface that Caddy shares via network_mode: service:tailscale. The layer
// manages the tailscale container's compose lifecycle (start/stop/status) and
// provides node identity information (tailnet IP, FQDN).
//
// Unlike other network layers, tailscale does NOT manage per-service tunnel
// config — it provides network-level connectivity. Service exposure through
// the tailnet is the default for `homelab enable <svc>`.
package tailscale

import (
	"fmt"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "tailscale"

// Layer implements network.NetworkLayer for the Tailscale mesh VPN node.
type Layer struct {
	repoRoot string
	runner   *run.Commander
	envFn    network.EnvFunc
}

// New creates a new Tailscale layer.
func New(repoRoot string, runner *run.Commander, envFn network.EnvFunc) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner, envFn: envFn}
}

// compile-time check
var _ network.NetworkLayer = (*Layer)(nil)

func (l *Layer) Name() string          { return "ts" }
func (l *Layer) Label() string         { return "Tailscale mesh VPN" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "" }

// Start brings the tailscale container up via its compose file.
func (l *Layer) Start() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot), l.env(),
		"up", "-d", containerName,
	)
}

// Stop brings the tailscale container down.
func (l *Layer) Stop() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot), l.env(),
		"stop", containerName,
	)
}

// Status returns the tailscale container state.
func (l *Layer) Status() network.Status {
	state := l.runner.ContainerStatus(containerName)
	return network.Status{ContainerState: state}
}

// Enable is a no-op for tailscale — tailnet connectivity is the default
// exposure layer for all services. Per-service routing is handled by Caddy
// config (conf.d/) which is written by the main enable command, not by this
// layer. The Enable signature is implemented for interface compliance but
// tailscale does not manage per-service tunnel configs.
func (l *Layer) Enable(_, _ string, _ network.ServiceInfo, _ []network.PortSelection) error {
	return nil
}

// Disable is a no-op for tailscale — removing tailnet connectivity is handled
// by stopping the tailscale container, not by per-service disable.
func (l *Layer) Disable(_ string) error {
	return nil
}

// CaddyConfigDir returns the standard conf.d/ directory — tailscale is the
// default exposure layer, so its Caddy configs go in the main conf.d/ dir.
func (l *Layer) CaddyConfigDir(configRoot string) string {
	return configRoot + "/caddy/conf.d"
}

// ServiceAddresses returns the tailnet hostname. Templated, not looked up:
// the wildcard cert and DNS record cover every *.<home>.<domain> name, so the
// name is fully determined by the service name and root config.
func (l *Layer) ServiceAddresses(svcName string, env map[string]string) []network.ServiceAddress {
	sub, dom := env["HOME_SUBDOMAIN"], env["DOMAIN"]
	if sub == "" || dom == "" {
		return []network.ServiceAddress{{Note: "HOME_SUBDOMAIN/DOMAIN not set — run homelab setup"}}
	}
	return []network.ServiceAddress{{URL: fmt.Sprintf("https://%s.%s.%s", svcName, sub, dom)}}
}

func (l *Layer) env() map[string]string {
	if l.envFn == nil {
		return map[string]string{}
	}
	return l.envFn()
}
