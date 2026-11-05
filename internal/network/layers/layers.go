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

// New returns every available layer, in display order, rooted at the given
// config dir. In cluster mode that is the tailnet and Cloudflare; tor, i2p and
// ygg stay out of the registry (see Unavailable) until a later plan brings them
// back. runner and env may be nil for callers that only read identity or
// routing (names, site templates) and never start a container.
func New(root string, runner *run.Commander, env network.EnvFunc) *network.Registry {
	r := network.NewRegistry()
	r.Register(tailscale.New(root, runner, env)) // the private layer: always first
	r.Register(cf.New(root, runner, env))
	return r
}

// Unavailable returns the layers that exist as packages but are not registered
// in cluster mode, in display order. With empty arguments they carry identity
// only (name, flag, label), which is what enable and disable use to refuse
// their flags with a clear message instead of "unknown flag". With a root and
// runner they are usable for Teardown, so `disable` can still clean up a
// service exposed on one before cluster mode.
func Unavailable(root string, runner *run.Commander, env network.EnvFunc) []network.NetworkLayer {
	return []network.NetworkLayer{
		tor.New(root, runner, env),
		i2p.New(root, runner, env),
		ygg.New(root, runner, env),
	}
}

// Legacy returns every layer that ever existed — New's plus Unavailable's —
// for reading old state: migration infers exposure.yaml from the legacy files
// of all five. It is never used to render sites; Sync renders what New serves.
func Legacy(root string, runner *run.Commander, env network.EnvFunc) *network.Registry {
	r := New(root, runner, env)
	for _, l := range Unavailable(root, runner, env) {
		r.Register(l)
	}
	return r
}

// Static returns the layers for identity only — name, label, flag —
// which do not depend on a config dir. For use where none is known yet, such
// as registering CLI flags at init.
func Static() []network.NetworkLayer { return New("", nil, nil).All() }
