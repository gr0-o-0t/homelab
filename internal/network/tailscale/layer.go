// Package tailscale implements the NetworkLayer interface for the private
// tailnet: the default layer of `homelab enable <svc>`. The tailnet node is
// the leader agent's own tsnet node "homelab", and its Caddy serves the sites
// on it, so this layer has no container of its own: its state is the agent
// container's.
//
// Unlike other network layers, tailscale does NOT manage per-service tunnel
// config — it provides network-level connectivity.
package tailscale

import (
	"fmt"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

// containerName is the leader agent's container, which runs the tailnet node
// and Caddy (internal/bootstrap.AgentContainer).
const containerName = "homelab-agent"

// Layer implements network.NetworkLayer for the tailnet.
//
// Its sites are <host>.<home>.<domain>, served with the wildcard cert. It has
// no per-service daemon state, so it implements no network.Configurer.
type Layer struct {
	network.HostTemplate
	repoRoot string
	runner   *run.Commander
}

// New creates a new Tailscale layer. The env func is unused: the layer reads
// nothing from the environment but the ServiceAddresses argument.
func New(repoRoot string, runner *run.Commander, _ network.EnvFunc) *Layer {
	return &Layer{
		HostTemplate: network.HostTemplate{Suffix: ".{$HOME_SUBDOMAIN}.{$DOMAIN}", TLS: true, Bind: network.TailnetBind},
		repoRoot:     repoRoot, runner: runner,
	}
}

// compile-time check
var _ network.NetworkLayer = (*Layer)(nil)

func (l *Layer) Name() string          { return "ts" }
func (l *Layer) Label() string         { return "Tailscale mesh VPN" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Flag() string          { return "" } // the default: bare `homelab enable`

// Status returns the agent container's state.
func (l *Layer) Status() network.Status {
	if l.runner == nil {
		return network.Status{ContainerState: "not found"}
	}
	return network.Status{ContainerState: l.runner.ContainerStatus(containerName)}
}

// LegacyConfDir is the main conf.d/: the tailnet is the default exposure
// layer, and the Caddyfile imported this directory before there were others.
func (l *Layer) LegacyConfDir() string { return "conf.d" }

// ServiceAddresses returns the tailnet hostname. Templated, not looked up:
// the wildcard cert and DNS record cover every *.<home>.<domain> name. The
// label is the one the service's site block answers on — any --name or
// declared subdomain (vaultwarden → vault) — not the bare service name.
func (l *Layer) ServiceAddresses(svcName string, env map[string]string) []network.ServiceAddress {
	sub, dom := env["HOME_SUBDOMAIN"], env["DOMAIN"]
	if sub == "" || dom == "" {
		return []network.ServiceAddress{{Note: "HOME_SUBDOMAIN/DOMAIN not set — run homelab setup"}}
	}
	return []network.ServiceAddress{{URL: fmt.Sprintf("https://%s.%s.%s", configgen.ServiceHost(l.repoRoot, svcName), sub, dom)}}
}
