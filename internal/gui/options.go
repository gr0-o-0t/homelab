package gui

import (
	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/clistate"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
)

// Options is everything the GUI needs from the CLI. It reads nothing local:
// what it shows comes from Source — this binary's `status --json` and
// `backup --list --json`, forwarded to the leader — so it works from any PC.
// Every Source call is made off the render thread.
type Options struct {
	Source clistate.Source
	// Catalog is the embedded catalog's service names, for the entries not
	// installed yet.
	Catalog []string
	// AllLayers is every layer the CLI knows, enabled or not: identities
	// (name, label, flag) only, never asked for state.
	AllLayers []network.NetworkLayer
	CLI       []string
}

// ContainerState is one core container and its docker state ("" = absent).
type ContainerState struct{ Name, State string }

// agentContainer runs Caddy and the tailnet node (bootstrap.AgentContainer).
const agentContainer = "homelab-agent"

// loaded is one refresh's worth of state, as the CLI reported it.
type loaded struct {
	services    []service.Service
	core        []ContainerState
	layers      []string // configured: ts + enabled extensions
	layerObjs   []network.NetworkLayer
	exts        []extState
	backups     []backup.Listing
	backupsErr  string
	discoverErr string
}

// load asks the Source for everything the GUI shows: services (with the
// catalog entries not installed), the edge and extension states, the enabled
// layers and the backups.
func load(opt Options) loaded {
	var l loaded
	if opt.Source == nil {
		l.discoverErr = "no homelab binary configured"
		return l
	}
	st, err := opt.Source.Status()
	if err != nil {
		l.discoverErr = err.Error()
	} else {
		l.services = st.List(opt.Catalog)
		states := st.CoreStates(agentContainer)
		l.core = append(l.core, ContainerState{Name: agentContainer, State: states[agentContainer]})
		enabled := st.Enabled()
		for _, layer := range opt.AllLayers {
			name := layer.Name()
			if name == "ts" || enabled[name] {
				l.layers = append(l.layers, name)
				l.layerObjs = append(l.layerObjs, layer)
			}
			if name == "ts" {
				continue
			}
			l.core = append(l.core, ContainerState{Name: layer.ContainerName(), State: states[layer.ContainerName()]})
			l.exts = append(l.exts, extState{
				Name: name, Label: layer.Label(), Container: layer.ContainerName(),
				Enabled: enabled[name], Running: states[layer.ContainerName()] == "running",
			})
		}
	}
	bl, berr := opt.Source.Backups()
	l.backups = bl
	if berr != nil {
		l.backupsErr = berr.Error()
	}
	return l
}

// addressesOf is svc's addresses per layer it is on, as the CLI reported
// them, labelled for display.
func addressesOf(svc *service.Service, layers []network.NetworkLayer) []layerAddrs {
	var out []layerAddrs
	for _, l := range layers {
		if !svc.On(l.Name()) {
			continue
		}
		out = append(out, layerAddrs{Layer: l.Name(), Label: l.Label(), Addrs: svc.Addresses[l.Name()]})
	}
	return out
}

type layerAddrs struct {
	Layer, Label string
	Addrs        []network.ServiceAddress
}
