// Package cf implements the NetworkLayer interface for Cloudflare Tunnel
// (cloudflared). Manages cloudflared container lifecycle and public DNS
// route management for exposing services via Cloudflare's edge network.
package cf

import (
	"fmt"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const containerName = "cloudflared"

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
func (l *Layer) Profile() string       { return "tunnel" }
func (l *Layer) Flag() string          { return "cf" }

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

func (l *Layer) env() map[string]string {
	if l.envFn == nil {
		return map[string]string{}
	}
	return l.envFn()
}
