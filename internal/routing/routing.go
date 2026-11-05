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
	"slices"

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
	// Raw ports ride the tailnet only: cf and i2p carry HTTP alone, tor and
	// ygg forward raw ports through their own daemons.
	l4 := ""
	if st.On("ts") {
		if err := checkL4Free(root, svc, e); err != nil {
			return err
		}
		l4 = configgen.RenderL4(e)
	}
	if err := writeOrRemove(configgen.SitesFile(root, svc), content); err != nil {
		return err
	}
	return writeOrRemove(configgen.L4File(root, svc), l4)
}

// reservedL4 are the listeners Caddy's HTTP sites already hold in the tailnet
// namespace (the admin API is on localhost:2019).
var reservedL4 = []string{"tcp/:80", "tcp/:443", "tcp/:8081", "tcp/:2019"}

// l4Listeners is every "<proto>/:<port>" a resolved exposure's raw ports bind.
func l4Listeners(e configgen.Exposure) []string {
	var out []string
	for _, p := range e.Ports {
		if p.Listen == 0 {
			continue
		}
		for _, proto := range p.Protocols {
			out = append(out, fmt.Sprintf("%s/:%d", proto, p.Listen))
		}
	}
	return out
}

// checkL4Free fails when a raw listen port of svc is an HTTP listener or is
// already bound by another tailnet service. A listen port has no Host to route
// on, and caddy-l4 accepts two servers on one address without complaint —
// `caddy validate` passes and one service silently gets the other's traffic.
func checkL4Free(root, svc string, e configgen.Exposure) error {
	mine := l4Listeners(e)
	for _, l := range mine {
		if slices.Contains(reservedL4, l) {
			return fmt.Errorf("%s: listen port %s is Caddy's own; pick another in its config.yaml", svc, l)
		}
	}
	if len(mine) == 0 {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(root, "services", "*", exposure.FileName))
	if err != nil {
		return err
	}
	for _, m := range matches {
		other := filepath.Base(filepath.Dir(m))
		if other == svc {
			continue
		}
		st, err := exposure.Load(root, other)
		if err != nil || !st.On("ts") {
			continue
		}
		oe, err := Resolve(root, other, st)
		if err != nil {
			continue // a broken neighbour must not block this service
		}
		for _, l := range l4Listeners(oe) {
			if slices.Contains(mine, l) {
				return fmt.Errorf("%s: listen port %s is already used by %s; change one of their config.yaml ports", svc, l, other)
			}
		}
	}
	return nil
}

// writeOrRemove writes content to path, or removes path when content is empty.
func writeOrRemove(path, content string) error {
	if content == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", filepath.Base(path), err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// RemoveSites removes a service's sites file and layer4 file. An absent file
// is not an error.
func RemoveSites(root, svc string) error {
	return errors.Join(
		writeOrRemove(configgen.SitesFile(root, svc), ""),
		writeOrRemove(configgen.L4File(root, svc), ""),
	)
}
