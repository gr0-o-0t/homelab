package swarm

import (
	"context"
	"fmt"
	"slices"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
)

// findNetwork looks a network up by exact name (the API's name filter
// matches substrings).
func (d *Deployer) findNetwork(ctx context.Context, name string) (network.Summary, bool, error) {
	list, err := d.API.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
	if err != nil {
		return network.Summary{}, false, err
	}
	for _, n := range list {
		if n.Name == name {
			return n, true, nil
		}
	}
	return network.Summary{}, false, nil
}

// EnsureNetwork creates the shared home-services overlay: attachable, MTU
// 1230 (VXLAN inside WireGuard), not encrypted (the data path rides the
// tailnet; Docker's IPsec rules fail on iptables-nft). An existing overlay is
// kept; the Compose-era bridge of the same name is refused.
func (d *Deployer) EnsureNetwork(ctx context.Context) error {
	n, found, err := d.findNetwork(ctx, SharedNetwork)
	if err != nil {
		return err
	}
	if found {
		if n.Driver != OverlayDriver {
			return fmt.Errorf("network %s exists with driver %q, not %s: it is the Compose-era network — "+
				"run `homelab down` (every service, then the core) and `docker network rm %s`, then retry",
				SharedNetwork, n.Driver, OverlayDriver, SharedNetwork)
		}
		return nil
	}
	_, err = d.API.NetworkCreate(ctx, SharedNetwork, network.CreateOptions{
		Driver:     OverlayDriver,
		Scope:      "swarm",
		Attachable: true,
		Options:    map[string]string{OverlayMTUOpt: OverlayMTU},
	})
	if err != nil && !errdefs.IsConflict(err) {
		return fmt.Errorf("creating network %s: %w", SharedNetwork, err)
	}
	return nil
}

// ensureStackNetworks creates the stack's own overlays (its internal
// database networks and the like) that do not exist yet.
func (d *Deployer) ensureStackNetworks(ctx context.Context, st Stack) error {
	for _, ns := range st.Networks {
		_, found, err := d.findNetwork(ctx, ns.Name)
		if err != nil {
			return err
		}
		if found {
			continue
		}
		_, err = d.API.NetworkCreate(ctx, ns.Name, network.CreateOptions{
			Driver:   ns.Driver,
			Scope:    "swarm",
			Internal: ns.Internal,
			Options:  ns.Options,
			Labels:   ns.Labels,
		})
		if err != nil && !errdefs.IsConflict(err) {
			return fmt.Errorf("creating network %s: %w", ns.Name, err)
		}
	}
	return nil
}

// checkAliases refuses a stack whose names on home-services (compose key,
// container_name) another deployed service already answers to: on one
// overlay, both would resolve and traffic would split between them (R10).
func (d *Deployer) checkAliases(ctx context.Context, st Stack) error {
	shared := []string{SharedNetwork}
	if n, found, err := d.findNetwork(ctx, SharedNetwork); err != nil {
		return err
	} else if found {
		shared = append(shared, n.ID) // the daemon stores attachments by ID
	}
	all, err := d.API.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelService)),
	})
	if err != nil {
		return err
	}
	owner := map[string]string{}
	for _, s := range all {
		other := s.Spec.Labels[LabelService]
		if other == st.Name {
			continue
		}
		for _, alias := range sharedAliases(s.Spec, shared) {
			owner[alias] = other
		}
	}
	for _, spec := range st.Services {
		for _, alias := range sharedAliases(spec, shared) {
			if other, taken := owner[alias]; taken {
				return fmt.Errorf("%s: the name %q on %s is already used by service %s — "+
					"rename the compose service or its container_name in one of them",
					st.Name, alias, SharedNetwork, other)
			}
		}
	}
	return nil
}

func sharedAliases(spec swarm.ServiceSpec, shared []string) []string {
	var out []string
	for _, a := range spec.TaskTemplate.Networks {
		if slices.Contains(shared, a.Target) {
			out = append(out, a.Aliases...)
		}
	}
	return out
}
