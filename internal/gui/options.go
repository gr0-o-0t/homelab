package gui

import (
	"sync/atomic"

	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/clistate"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
)

// Options is everything the GUI needs from the CLI. It reads nothing local:
// what it shows comes from Source — this binary's `status --json` and
// `backup-list --json`, forwarded to the leader — so it works from any PC.
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

// loaded is one status read's worth of state, as the CLI reported it.
type loaded struct {
	services    []service.Service
	core        []ContainerState
	layers      []string // configured: ts + enabled extensions
	layerObjs   []network.NetworkLayer
	exts        []extState
	discoverErr string
}

// load asks the Source for everything the GUI shows: services (with the
// catalog entries not installed), the edge and extension states, the enabled
// layers. Backups are read apart (loadBackups).
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
	return l
}

// loadBackups lists the leader's backups ("" or the error).
func loadBackups(opt Options) ([]backup.Listing, string) {
	if opt.Source == nil {
		return nil, "no homelab binary configured"
	}
	bl, err := opt.Source.Backups()
	if err != nil {
		return nil, err.Error()
	}
	return bl, ""
}

// flight is one kind of background read: a second does not start while one
// is in flight, so a slow or hung leader piles nothing up.
type flight struct{ busy atomic.Bool }

func (f *flight) begin() bool { return f.busy.CompareAndSwap(false, true) }
func (f *flight) end()        { f.busy.Store(false) }

// refresher runs the GUI's two reads: the status, which the caller waits
// for, and the backups listing, started in the background so a slow listing
// never delays the status. Each is skipped while a previous one runs.
type refresher struct{ status, backups flight }

func (r *refresher) run(status, backups func()) {
	if r.backups.begin() {
		go func() {
			defer r.backups.end()
			backups()
		}()
	}
	if r.status.begin() {
		defer r.status.end()
		status()
	}
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
