// Package routing exposes a service on network layers and withdraws it again.
//
// A service's exposure is stored in services/<name>/exposure.yaml (see
// internal/exposure) and rendered into one Caddy file, caddy/sites/<name>.conf,
// holding the site blocks of every layer it is on. `homelab enable`,
// `disable`, `delete` and `reload <svc>` all go through here: change the
// state, re-render that one file, reload Caddy. A layer's daemon side
// (network.Configurer: torrc.d, tunnels.conf, socat.d) stays the layer's own,
// configured and torn down through Configure and Teardown.
//
// Nothing here reloads Caddy; callers reload once after their writes.
package routing

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
)

// Resolve reads a service's declaration into an Exposure for the --name and
// --ports recorded in its state.
func Resolve(root, svc string, st exposure.State) (configgen.Exposure, error) {
	return configgen.Resolve(root, svc, st.Name, st.Ports)
}

// Configure sets a service up on a layer's daemon, if the layer has one.
//
// It runs before the layer is recorded in the state, and before the sites file
// is rendered. That order is deliberate: the daemon side is the half that can
// fail on environment — a root-owned tor key directory, a stopped daemon — and
// some site addresses (an onion, a mesh port) exist only once it is done.
func Configure(l network.NetworkLayer, svc string, st exposure.State, e configgen.Exposure) error {
	c, ok := l.(network.Configurer)
	if !ok {
		return nil
	}
	name := st.Name
	if name == "" {
		name = svc
	}
	return c.Configure(svc, name, e.Ports)
}

// Teardown removes a service from a layer's daemon, if the layer has one.
func Teardown(l network.NetworkLayer, svc string) error {
	if c, ok := l.(network.Configurer); ok {
		return c.Teardown(svc)
	}
	return nil
}

// Apply changes a service's stored state with fn and re-renders its sites
// file from the result. On error the files may be half-written; callers that
// need atomicity snapshot them first (internal/caddy.Snapshot).
func Apply(root string, reg *network.Registry, svc string, fn func(*exposure.State)) error {
	if err := exposure.Update(root, svc, fn); err != nil {
		return err
	}
	return Sync(root, reg, svc)
}

// Sync renders a service's sites file from its stored state, or removes it
// when the service is on no layer.
func Sync(root string, reg *network.Registry, svc string) error {
	st, err := exposure.Load(root, svc)
	if err != nil {
		return err
	}
	var on []network.NetworkLayer
	for _, l := range reg.All() {
		if st.On(l.Name()) {
			on = append(on, l)
		}
	}
	if len(on) == 0 {
		return RemoveSites(root, svc)
	}
	e, err := Resolve(root, svc, st)
	if err != nil {
		return err
	}
	content, err := configgen.RenderSites(on, e)
	if err != nil {
		return err
	}
	path := configgen.SitesFile(root, svc)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// RemoveSites removes a service's sites file. An absent file is not an error.
func RemoveSites(root, svc string) error {
	if err := os.Remove(configgen.SitesFile(root, svc)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing sites file: %w", err)
	}
	return nil
}
