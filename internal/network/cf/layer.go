// Package cf implements the NetworkLayer interface for Cloudflare Tunnel
// (cloudflared). cloudflared runs as a Swarm service (swarm.Deployer); the
// tunnel's ingress points at http://homelab-edge:80, the agent's plain-HTTP
// listener.
package cf

import (
	"fmt"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

// containerName is the Swarm service; there is no fixed container name.
const containerName = "homelab_cloudflared"

// Layer implements network.NetworkLayer for Cloudflare Tunnel.
//
// Sites are plain http://<host>.<domain>: TLS is terminated at the Cloudflare
// edge. cloudflared's ingress is managed by `homelab cf route`, so there is no
// per-service daemon state and no network.Configurer.
type Layer struct {
	network.HostTemplate
	repoRoot string
	state    func() string
}

// New creates a new Cloudflare layer. The runner and env func are unused:
// cloudflared is deployed by swarm.Deployer, not by the layer.
func New(repoRoot string, _ *run.Commander, _ network.EnvFunc) *Layer {
	return &Layer{
		HostTemplate: network.HostTemplate{Prefix: "http://", Suffix: ".{$DOMAIN}"},
		repoRoot:     repoRoot,
	}
}

var _ network.NetworkLayer = (*Layer)(nil)

func (l *Layer) Name() string          { return "cf" }
func (l *Layer) Label() string         { return "Cloudflare Tunnel" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Flag() string          { return "cf" }

// WithState sets where Status reads cloudflared's state: its Swarm task, not
// a container of its own.
func (l *Layer) WithState(fn func() string) *Layer { l.state = fn; return l }

func (l *Layer) Status() network.Status {
	if l.state == nil {
		return network.Status{ContainerState: "not found"}
	}
	return network.Status{ContainerState: l.state()}
}

func (l *Layer) LegacyConfDir() string { return "conf.d-cf" }

// ServiceAddresses returns the public hostname Cloudflare fronts: the host the
// service's site block answers on, which carries any --name or declared
// subdomain — not the bare service name, which matched no site block whenever
// those were set.
func (l *Layer) ServiceAddresses(svcName string, env map[string]string) []network.ServiceAddress {
	dom := env["DOMAIN"]
	if dom == "" {
		return []network.ServiceAddress{{Note: "DOMAIN not set — run homelab setup"}}
	}
	host := configgen.ServiceHost(l.repoRoot, svcName)
	return []network.ServiceAddress{{URL: fmt.Sprintf("https://%s.%s", host, dom)}}
}
