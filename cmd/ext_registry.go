package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/network/cf"
	"github.com/groot/homelab/internal/network/i2p"
	"github.com/groot/homelab/internal/network/ipfs"
	"github.com/groot/homelab/internal/network/tailscale"
	"github.com/groot/homelab/internal/network/tor"
	"github.com/groot/homelab/internal/network/ygg"
)

// extRegistry is the global network layer registry. Populated in init().
var extRegistry *network.Registry

// initExtensions registers all built-in network layers into the global registry.
func initExtensions() {
	extRegistry = network.NewRegistry()
	root := configDir()

	// Register layers in display order (default-enabled first)
	extRegistry.Register(tailscale.New(root, run.Default()))
	extRegistry.Register(cf.New(root, run.Default()))
	extRegistry.Register(tor.New(root, run.Default()))
	extRegistry.Register(i2p.New(root, run.Default()))
	extRegistry.Register(ygg.New(root, run.Default()))
	extRegistry.Register(ipfs.New(root, run.Default()))
}

// extCommandFor creates a root-level cobra command that delegates lifecycle
// and status operations to the named network layer in the registry.
func extCommandFor(name string) *cobra.Command {
	layer, ok := extRegistry.Get(name)
	if !ok {
		return nil
	}

	cmd := &cobra.Command{
		Use:   name,
		Short: fmt.Sprintf("Manage %s", layer.Label()),
		Long:  fmt.Sprintf("Manage the %s network extension layer.", layer.Label()),
	}

	cmd.AddCommand(&cobra.Command{
		Use:     "status",
		Aliases: []string{"ps"},
		Short:   fmt.Sprintf("Show %s status", layer.Label()),
		RunE: func(cmd *cobra.Command, args []string) error {
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
		Short: fmt.Sprintf("Stream %s container logs", layer.Label()),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := configDir()
			env := buildEnv(root, "")
			return run.Default().DockerComposeEnv(
				run.CoreComposeFile(root),
				env,
				withProfiles(root, "logs", "-f", layer.ContainerName())...,
			)
		},
	})

	return cmd
}
