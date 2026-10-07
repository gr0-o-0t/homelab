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

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "tailscale"

// Layer implements network.NetworkLayer for the Tailscale mesh VPN node.
//
// Its sites are <host>.<home>.<domain>, served with the wildcard cert. It has
// no per-service daemon state, so it implements no network.Configurer.
type Layer struct {
	network.HostTemplate
	repoRoot string
	runner   *run.Commander
	envFn    network.EnvFunc
}

// New creates a new Tailscale layer.
func New(repoRoot string, runner *run.Commander, envFn network.EnvFunc) *Layer {
	return &Layer{
		HostTemplate: network.HostTemplate{Suffix: ".{$HOME_SUBDOMAIN}.{$DOMAIN}", TLS: true},
		repoRoot:     repoRoot, runner: runner, envFn: envFn,
	}
}

// compile-time check
var _ network.NetworkLayer = (*Layer)(nil)

func (l *Layer) Name() string          { return "ts" }
func (l *Layer) Label() string         { return "Tailscale mesh VPN" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "" }
func (l *Layer) Flag() string          { return "" } // the default: bare `homelab enable`

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

// ConfDir is the main conf.d/: the tailnet is the default exposure layer, and
// the Caddyfile imported this directory before there were other layers.
func (l *Layer) ConfDir() string { return "conf.d" }

// ServiceAddresses returns the tailnet hostname. Templated, not looked up:
// the wildcard cert and DNS record cover every *.<home>.<domain> name. The
// label is the one the generated conf.d block answers on — any --name or
// declared subdomain (vaultwarden → vault) — not the bare service name.
func (l *Layer) ServiceAddresses(svcName string, env map[string]string) []network.ServiceAddress {
	sub, dom := env["HOME_SUBDOMAIN"], env["DOMAIN"]
	if sub == "" || dom == "" {
		return []network.ServiceAddress{{Note: "HOME_SUBDOMAIN/DOMAIN not set — run homelab setup"}}
	}
	return []network.ServiceAddress{{URL: fmt.Sprintf("https://%s.%s.%s", configgen.CurrentHost(l.repoRoot, l, svcName), sub, dom)}}
}

func (l *Layer) env() map[string]string {
	if l.envFn == nil {
		return map[string]string{}
	}
	return l.envFn()
}
