package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/clistate"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/edge"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/swarm"
)

// `homelab status --json` is everything the front ends display, as one
// document (internal/clistate): run through this binary it is forwarded to
// the leader, so the dashboard and the GUI show the cluster from any PC.
// `homelab status <svc> --json` is one service, with its tasks.

func writeStatusJSON(dir string, args []string, w io.Writer) error {
	env := buildEnv(dir, "")
	svcs, err := discoverServices(dir)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if len(args) > 0 {
		for i := range svcs {
			if svcs[i].Name == args[0] {
				return enc.Encode(clistate.FromService(&svcs[i], serviceAddresses(&svcs[i], env)))
			}
		}
		return fmt.Errorf("service %q not found", args[0])
	}

	st := clistate.Status{
		Version:          Version,
		StateUnavailable: stateUnavailable(svcs),
		Nodes:            clusterNodes(),
		Extensions:       []clistate.Extension{},
		Services:         make([]clistate.Service, 0, len(svcs)),
	}
	if err := edge.Reachable(edge.AdminSocket(dir)); err != nil {
		st.Edge.Error = err.Error()
	} else {
		st.Edge.Reachable = true
	}
	cfg, _ := config.Load(rootConfigFile())
	for _, l := range extRegistry().All() {
		if l.Flag() == "" {
			continue
		}
		st.Extensions = append(st.Extensions, clistate.Extension{
			Name: l.Name(), Label: l.Label(), Container: l.ContainerName(),
			Enabled: hasResolvedExtension(cfg, l.Name()), State: l.Status().ContainerState,
		})
	}
	if cfg != nil {
		for g := range cfg.Groups {
			st.Groups = append(st.Groups, g)
		}
		slices.Sort(st.Groups)
	}
	for i := range svcs {
		st.Services = append(st.Services, clistate.FromService(&svcs[i], serviceAddresses(&svcs[i], env)))
	}
	return enc.Encode(st)
}

// serviceAddresses is every address svc answers on, per layer it is on.
func serviceAddresses(svc *service.Service, env map[string]string) map[string][]network.ServiceAddress {
	out := map[string][]network.ServiceAddress{}
	for _, name := range svc.ActiveLayers() {
		if l, ok := extRegistry().Get(name); ok {
			out[name] = l.ServiceAddresses(svc.Name, env)
		}
	}
	return out
}

// clusterNodes lists the Swarm nodes; empty when this is not a manager or
// Docker cannot be asked.
func clusterNodes() []clistate.Node {
	out := []clistate.Node{}
	_ = withSwarmAPI(func(api swarm.API) error {
		ctx, cancel := context.WithTimeout(cmdContext(), 5*time.Second)
		defer cancel()
		nodes, err := api.NodeList(ctx, dswarm.NodeListOptions{})
		if err != nil {
			return err
		}
		for i := range nodes {
			n := &nodes[i]
			out = append(out, clistate.Node{
				Hostname: n.Description.Hostname, Role: n.Spec.Labels[swarm.LabelNodeRole],
				State: string(n.Status.State), Availability: string(n.Spec.Availability),
				Manager: n.ManagerStatus != nil, Leader: n.ManagerStatus != nil && n.ManagerStatus.Leader,
			})
		}
		return nil
	})
	return out
}
