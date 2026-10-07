// Package routing exposes a service on a network layer and withdraws it again.
//
// It is the one path `homelab enable`, `disable`, `delete` and enable's
// rollback go through, for every layer alike: the layer's daemon side
// (network.Configurer) first, then the Caddy blocks configgen renders for it.
// Neither function reloads Caddy: the callers batch every layer's writes and
// reload once.
package routing

import (
	"fmt"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
)

// Enable exposes a service on one layer.
//
// displayName overrides the host label (empty means the service name), and
// portNames restricts which declared ports are exposed (empty means all).
//
// The layer's own config (tunnel, hidden service, forwarder) is written before
// the Caddy blocks. That order is deliberate: the layer is the half that can
// fail on environment — a root-owned tor key directory, a stopped daemon — and
// doing it second used to leave a Caddy block behind for a layer that was
// never configured, which `homelab status` then reported as an exposure.
func Enable(root string, l network.NetworkLayer, svcName, displayName string, portNames []string) error {
	e, err := configgen.Resolve(root, svcName, displayName, portNames)
	if err != nil {
		return err
	}
	if c, ok := l.(network.Configurer); ok {
		name := displayName
		if name == "" {
			name = svcName
		}
		if err := c.Configure(svcName, name, e.Ports); err != nil {
			return err
		}
	}
	return writeBlocks(root, l, e)
}

// Refresh rewrites a layer's Caddy blocks from an already-resolved exposure,
// without touching the layer's daemon.
func Refresh(root string, l network.NetworkLayer, e configgen.Exposure) error {
	return writeBlocks(root, l, e)
}

func writeBlocks(root string, l network.NetworkLayer, e configgen.Exposure) error {
	blocks, err := configgen.Render(l, e)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if err := configgen.WriteFile(root, l.ConfDir(), e.Service, b.PortName, b.Content); err != nil {
			return fmt.Errorf("writing %s config: %w", l.Name(), err)
		}
	}
	return nil
}

// RemoveRoutes removes a service's Caddy blocks for a layer. An already-absent
// route is not an error.
func RemoveRoutes(root string, l network.NetworkLayer, svcName string) error {
	return configgen.RemoveAllPortFiles(root, l.ConfDir(), svcName)
}

// Disable withdraws a service from one layer: its Caddy blocks, then the
// layer's own config. A failure to tear down the daemon side is ignored, as
// it always was — the route is what exposes the service.
func Disable(root string, l network.NetworkLayer, svcName string) error {
	if err := RemoveRoutes(root, l, svcName); err != nil {
		return err
	}
	if c, ok := l.(network.Configurer); ok {
		_ = c.Teardown(svcName)
	}
	return nil
}
