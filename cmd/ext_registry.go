package cmd

import (
	"fmt"
	"sync"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/network/cf"
	"github.com/groot/homelab/internal/network/i2p"
	"github.com/groot/homelab/internal/network/tailscale"
	"github.com/groot/homelab/internal/network/tor"
	"github.com/groot/homelab/internal/network/ygg"
)

var (
	extRegistryOnce     sync.Once
	extRegistryInstance *network.Registry
)

// extRegistry lazily builds and returns the network layer registry, on first
// use rather than at package init(). Go runs all init() funcs before Cobra
// parses os.Args, so building this eagerly from init() would permanently
// freeze every layer's repoRoot to the XDG default, silently ignoring
// --config-dir/--config. Every real call site is inside a RunE (i.e. after
// flags are parsed), so lazy construction here fixes that.
func extRegistry() *network.Registry {
	extRegistryOnce.Do(func() {
		extRegistryInstance = network.NewRegistry()
		root := configDir()

		// Evaluated per compose call, not here: buildEnv reads the keyring.
		env := func() map[string]string { return buildEnv(root, "") }

		// Register layers in display order (default-enabled first)
		extRegistryInstance.Register(tailscale.New(root, run.Default(), env))
		extRegistryInstance.Register(cf.New(root, run.Default(), env))
		extRegistryInstance.Register(tor.New(root, run.Default(), env))
		extRegistryInstance.Register(i2p.New(root, run.Default(), env))
		extRegistryInstance.Register(ygg.New(root, run.Default(), env))
	})
	return extRegistryInstance
}

// extLabels are static display labels for extensions whose command tree is
// built at package-init time (extCommandFor), before flags are parsed and
// therefore before the registry above may safely be constructed.
var extLabels = map[string]string{
	"ts": "Tailscale",
}

// extCommandFor creates a root-level cobra command that delegates lifecycle
// and status operations to the named network layer in the registry. The
// layer is looked up lazily inside each RunE, never at command-tree
// construction time, so --config-dir/--config are honored.
func extCommandFor(name string) *cobra.Command {
	label, ok := extLabels[name]
	if !ok {
		return nil
	}

	cmd := &cobra.Command{
		Use:   name,
		Short: fmt.Sprintf("Manage %s", label),
		Long:  fmt.Sprintf("Manage the %s network extension layer.", label),
	}

	cmd.AddCommand(&cobra.Command{
		Use:     "status",
		Aliases: []string{"ps"},
		Short:   fmt.Sprintf("Show %s status", label),
		RunE: func(cmd *cobra.Command, args []string) error {
			layer, ok := extRegistry().Get(name)
			if !ok {
				return fmt.Errorf("extension %q not registered", name)
			}
			status := layer.Status()
			fmt.Printf("\n%s\n\n", styles.Header.Render(layer.Label()))
			switch status.ContainerState {
			case "running":
				fmt.Printf("  %s  %s  %s\n", styles.Success.Render("✓"), styles.Bold.Render(layer.Name()), styles.StateTag(status.ContainerState))
			case "not found":
				fmt.Printf("  %s  %s  %s\n", styles.Err.Render("✗"), styles.Bold.Render(layer.Name()), styles.Muted.Render("not installed"))
			default:
				fmt.Printf("  %s  %s  %s\n", styles.Warning.Render("!"), styles.Bold.Render(layer.Name()), styles.StateTag(status.ContainerState))
			}
			fmt.Println()
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "logs",
		Short: fmt.Sprintf("Stream %s container logs", label),
		RunE: func(cmd *cobra.Command, args []string) error {
			layer, ok := extRegistry().Get(name)
			if !ok {
				return fmt.Errorf("extension %q not registered", name)
			}
			return coreCompose("logs", "-f", layer.ContainerName())
		},
	})

	return cmd
}
