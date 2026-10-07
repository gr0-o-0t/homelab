// Package layers builds the network layer registry — the only list of layers.
// Commands, discovery, the Caddy snapshot, the TUI and the GUI all iterate it,
// so adding a layer means writing its package and one Register line here.
package layers

import (
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/cf"
	"github.com/groot/homelab/internal/network/i2p"
	"github.com/groot/homelab/internal/network/tailscale"
	"github.com/groot/homelab/internal/network/tor"
	"github.com/groot/homelab/internal/network/ygg"
	"github.com/groot/homelab/internal/run"
)

// New returns every layer, in display order, rooted at the given config dir.
// runner and env may be nil for callers that only read identity or routing
// (names, site templates) and never start a container.
func New(root string, runner *run.Commander, env network.EnvFunc) *network.Registry {
	r := network.NewRegistry()
	r.Register(tailscale.New(root, runner, env)) // the private layer: always first
	r.Register(cf.New(root, runner, env))
	r.Register(tor.New(root, runner, env))
	r.Register(i2p.New(root, runner, env))
	r.Register(ygg.New(root, runner, env))
	return r
}

// Static returns the layers for identity only — name, label, flag —
// which do not depend on a config dir. For use where none is known yet, such
// as registering CLI flags at init.
func Static() []network.NetworkLayer { return New("", nil, nil).All() }
