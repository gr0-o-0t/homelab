package swarm

import (
	"maps"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/docker/api/types/swarm"
)

// SharedNetwork is the cluster-wide overlay Caddy and the shared databases
// reach services on.
const SharedNetwork = "home-services"

// Overlay settings for every network homelab creates: VXLAN inside
// WireGuard needs the smaller MTU, and the data path rides the tailnet, so
// overlays are not encrypted (see Global Constraints, R11).
const (
	OverlayDriver = "overlay"
	OverlayMTUOpt = "com.docker.network.driver.mtu"
	OverlayMTU    = "1230"
)

// NetworkSpec is a stack-private overlay the deployer creates.
type NetworkSpec struct {
	Name     string
	Internal bool
	Driver   string
	Options  map[string]string
	Labels   map[string]string
}

type networkPlan struct {
	specs  []NetworkSpec
	target map[string]string // compose network key → Swarm network name
}

// planNetworks names every compose network. External ones keep their name;
// the stack's own become <svc>_<key> overlays, except that a stack with a
// single internal network calls it <svc>_internal.
func planNetworks(stack string, proj *types.Project) networkPlan {
	internal := 0
	for _, n := range proj.Networks {
		if n.Internal && !bool(n.External) {
			internal++
		}
	}
	p := networkPlan{target: map[string]string{}}
	for _, key := range slices.Sorted(maps.Keys(proj.Networks)) {
		n := proj.Networks[key]
		if n.External {
			name := n.Name
			if name == "" {
				name = key
			}
			p.target[key] = name
			continue
		}
		name := stack + "_" + key
		if n.Internal && internal == 1 {
			name = stack + "_internal"
		}
		p.target[key] = name
		p.specs = append(p.specs, NetworkSpec{
			Name:     name,
			Internal: n.Internal,
			Driver:   OverlayDriver,
			Options:  map[string]string{OverlayMTUOpt: OverlayMTU},
			Labels:   map[string]string{LabelNamespace: stack, LabelService: stack},
		})
	}
	slices.SortFunc(p.specs, func(a, b NetworkSpec) int { return strings.Compare(a.Name, b.Name) })
	return p
}

// attachments lists the networks a container joins. Only containers compose
// puts on home-services join it, aliased by compose key and container_name
// (Swarm adds the bare key anyway; names must be catalog-unique, R10).
func (p networkPlan) attachments(svc types.ServiceConfig) []swarm.NetworkAttachmentConfig {
	var out []swarm.NetworkAttachmentConfig
	for _, key := range slices.Sorted(maps.Keys(svc.Networks)) {
		target := p.target[key]
		var aliases []string
		if target == SharedNetwork {
			aliases = append(aliases, svc.Name, svc.ContainerName)
		}
		if c := svc.Networks[key]; c != nil {
			aliases = append(aliases, c.Aliases...)
		}
		out = append(out, swarm.NetworkAttachmentConfig{Target: target, Aliases: dedupe(aliases)})
	}
	return out
}

func dedupe(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
