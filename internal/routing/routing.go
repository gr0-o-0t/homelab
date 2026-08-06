// Package routing decides how a service's private (tailnet) Caddy route is
// written, and hides that decision from everything that just wants it enabled.
//
// There are two ways a route can exist, for historical reasons:
//
//   - a symlink from caddy/conf.d/<svc>.conf to a static caddy.conf the
//     service ships (the original scheme, internal/caddy)
//   - a file generated from the service's declared ports or its
//     caddy.routes.conf (the current scheme, internal/configgen)
//
// Which one applies depends on what the service ships, and every caller used
// to re-derive that: `homelab enable`, `homelab disable`, `homelab delete` and
// the TUI each had their own version of the rule. The TUI's was simply wrong —
// it only ever tried the symlink, so enabling a service that declares ports
// instead of shipping a caddy.conf did nothing there while working fine from
// the CLI.
//
// One rule, four callers. When the catalog finishes migrating off static
// caddy.conf files, the symlink branch disappears from this file alone.
package routing

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/run"
)

// EnablePrivate writes the private tailnet route for a service.
//
// displayName overrides the subdomain (empty means the service name), and
// portNames restricts which declared ports get routed (empty means all).
// Pass a Commander to capture Caddy's reload output; nil uses the default.
func EnablePrivate(root, svcName, displayName string, portNames []string, r *run.Commander) error {
	info, err := configgen.LoadServiceInfo(root, svcName)
	if err != nil {
		return err
	}

	// A caddy.routes.conf is the single source of truth for every layer, so it
	// takes precedence over a symlinked caddy.conf — otherwise the private
	// layer would be served from a second, drifting copy of the same routes.
	if info.Routes == "" {
		if hasStaticConf(root, svcName) {
			return manager(root, r).Enable(svcName)
		}
		if len(info.Ports) == 0 {
			return fmt.Errorf(
				"no ports defined in config.yaml and no caddy.conf or %s found for %s",
				configgen.RoutesFileName, svcName)
		}
	}

	blocks, err := configgen.Generate(configgen.Request{
		ServiceName: svcName,
		DisplayName: displayName,
		Extensions:  []string{"private"},
		PortNames:   portNames,
		ConfigDir:   root,
	})
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if b.Content == "" {
			continue // layer writes its own config; see cmd/enable.go
		}
		if err := configgen.WriteFile(root, b.Extension, svcName, b.PortName, b.Content); err != nil {
			return fmt.Errorf("writing private config: %w", err)
		}
	}
	return nil
}

// DisablePrivate removes the private route in whichever form it exists.
//
// Both forms are attempted, and an already-absent route is not an error: this
// is called from `delete`, where the goal is only that nothing is left behind.
func DisablePrivate(root, svcName string, r *run.Commander) error {
	symlinkErr := manager(root, r).Disable(svcName)
	generatedErr := configgen.RemoveAllPortFiles(root, "private", svcName)
	if symlinkErr != nil && generatedErr != nil {
		return fmt.Errorf("service %q has no active private route", svcName)
	}
	return nil
}

// hasStaticConf reports whether the service ships a hand-written caddy.conf.
func hasStaticConf(root, svcName string) bool {
	_, err := os.Stat(filepath.Join(root, "services", svcName, "caddy.conf"))
	return err == nil
}

func manager(root string, r *run.Commander) *caddy.Manager {
	if r == nil {
		return caddy.New(root)
	}
	return caddy.NewWithRunner(root, r)
}
