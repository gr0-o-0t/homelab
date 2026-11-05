// Package cf implements the NetworkLayer interface for Cloudflare Tunnel
// (cloudflared). cloudflared runs as a Swarm service (swarm.Deployer); the
// tunnel's ingress points at http://homelab-edge:80, the agent's plain-HTTP
// listener.
package cf

import (
	"errors"
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
	runner   *run.Commander
	envFn    network.EnvFunc
	state    func() string
}

// New creates a new Cloudflare layer.
func New(repoRoot string, runner *run.Commander, envFn network.EnvFunc) *Layer {
	return &Layer{
		HostTemplate: network.HostTemplate{Prefix: "http://", Suffix: ".{$DOMAIN}"},
		repoRoot:     repoRoot, runner: runner, envFn: envFn,
	}
}

var _ network.NetworkLayer = (*Layer)(nil)

func (l *Layer) Name() string          { return "cf" }
func (l *Layer) Label() string         { return "Cloudflare Tunnel" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "" } // no compose profile: it is a Swarm service
func (l *Layer) Flag() string          { return "cf" }

// WithState sets where Status reads cloudflared's state: its Swarm task, not
// a compose container.
func (l *Layer) WithState(fn func() string) *Layer { l.state = fn; return l }

var errSwarmService = errors.New("cloudflared runs as a Swarm service — use homelab ext start cf")

// Start and Stop are not how cloudflared runs: it is a Swarm service,
// deployed with `homelab ext start|stop cf`.
func (l *Layer) Start() error { return errSwarmService }
func (l *Layer) Stop() error  { return errSwarmService }

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
