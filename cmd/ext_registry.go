package cmd

import (
	"fmt"
	"strings"
	"sync"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
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
		root := configDir()
		// Evaluated per compose call, not here: buildEnv reads the keyring.
		env := func() map[string]string { return buildEnv(root, "") }
		extRegistryInstance = layers.New(root, run.Default(), env)
	})
	return extRegistryInstance
}

// layerByName returns a registered layer, accepting config.yaml aliases
// ("yggdrasil" for ygg).
func layerByName(name string) (network.NetworkLayer, error) {
	l, ok := extRegistry().Get(config.ResolveExtension(name))
	if !ok {
		return nil, fmt.Errorf("unknown extension %q\n\nAvailable: %s", name, strings.Join(extNames(), ", "))
	}
	return l, nil
}

// extLabel is a layer's display label, or the name itself when unknown.
func extLabel(name string) string {
	if l, err := layerByName(name); err == nil {
		return l.Label()
	}
	return name
}

// optionalLayers are the layers an `ext` command can switch on and off: all
// but the private tailnet, which is part of the core stack.
func optionalLayers() []network.NetworkLayer {
	var out []network.NetworkLayer
	for _, l := range extRegistry().All() {
		if l.Flag() != "" {
			out = append(out, l)
		}
	}
	return out
}

// extNames lists the optional layers' names.
func extNames() []string {
	var names []string
	for _, l := range optionalLayers() {
		names = append(names, l.Name())
	}
	return names
}

// extCommandFor creates a root-level cobra command that delegates lifecycle
// and status operations to the named network layer in the registry. Only the
// label is read at construction time (from layers.Static, which needs no
// config dir); the layer itself is looked up inside each RunE, so
// --config-dir/--config are honored.
func extCommandFor(name string) *cobra.Command {
	label := ""
	for _, l := range layers.Static() {
		if l.Name() == name {
			label = l.Label()
		}
	}
	if label == "" {
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
