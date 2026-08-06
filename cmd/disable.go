package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/routing"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var disableCmd = &cobra.Command{
	Use:   "disable <service>",
	Short: "Disable network exposure for a service",
	Long: `Remove network exposure layers for a service.

Select which layers to remove with extension flags:

  --cf    remove Cloudflare Tunnel config
  --i2p   remove I2P eepsite config
  --tor   remove Tor onion service config
  --ygg   remove Yggdrasil mesh config

Without flags, only the private tailnet config is removed.
-a/--all removes every extension layer AND stops the container.
--stop stops the container without touching extension layers.

Examples:
  homelab disable gitea                  # private only
  homelab disable gitea --cf             # remove CF exposure
  homelab disable gitea --cf --i2p       # CF + I2P
  homelab disable gitea -a               # all layers + stop container
  homelab disable gitea --stop           # stop container only`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runDisable,
}

var (
	disableCf   bool
	disableI2P  bool
	disableTor  bool
	disableYgg  bool
	disableAll  bool // -a flag: all extension layers + stop container
	disableStop bool // --stop: stop container only, standalone
)

func init() {
	disableCmd.Flags().BoolVar(&disableCf, "cf", false, "Remove Cloudflare Tunnel config")
	disableCmd.Flags().BoolVar(&disableI2P, "i2p", false, "Remove I2P eepsite config")
	disableCmd.Flags().BoolVar(&disableTor, "tor", false, "Remove Tor onion service config")
	disableCmd.Flags().BoolVar(&disableYgg, "ygg", false, "Remove Yggdrasil mesh config")
	disableCmd.Flags().BoolVarP(&disableAll, "all", "a", false, "Remove ALL extension configs and stop the container")
	disableCmd.Flags().BoolVar(&disableStop, "stop", false, "Also stop the service container")
	rootCmd.AddCommand(disableCmd)
}

func runDisable(cmd *cobra.Command, args []string) error {
	svcName := args[0]
	root := configDir()

	// Determine which layers to remove
	if disableAll {
		disableCf = true
		disableI2P = true
		disableTor = true
		disableYgg = true
		disableStop = true
	}

	hasSpecific := disableCf || disableI2P || disableTor || disableYgg

	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Disable: %s", svcName)))

	// Private tailnet (removed when no specific ext flags, or always with -a)
	if !hasSpecific || disableAll {
		if err := routing.DisablePrivate(root, svcName, nil); err != nil {
			return err
		}
		fmt.Printf("  %s  Private: removed\n", styles.Warning.Render("→"))
	}

	// Extension layers (via registry for extension-specific tunnel config)
	if disableCf {
		if err := configgen.RemoveAllPortFiles(root, "cf", svcName); err != nil {
			return fmt.Errorf("cf: %w", err)
		}
		if layer, ok := extRegistry().Get("cf"); ok {
			_ = layer.Disable(svcName)
		}
		fmt.Printf("  %s  Cloudflare: removed\n", styles.Warning.Render("→"))
	}
	if disableI2P {
		if err := configgen.RemoveAllPortFiles(root, "i2p", svcName); err != nil {
			return fmt.Errorf("i2p: %w", err)
		}
		if layer, ok := extRegistry().Get("i2p"); ok {
			_ = layer.Disable(svcName)
		}
		fmt.Printf("  %s  I2P: removed\n", styles.Warning.Render("→"))
	}
	if disableTor {
		if err := configgen.RemoveAllPortFiles(root, "tor", svcName); err != nil {
			return fmt.Errorf("tor: %w", err)
		}
		if layer, ok := extRegistry().Get("tor"); ok {
			_ = layer.Disable(svcName)
		}
		fmt.Printf("  %s  Tor: removed\n", styles.Warning.Render("→"))
	}
	if disableYgg {
		if err := configgen.RemoveAllPortFiles(root, "ygg", svcName); err != nil {
			return fmt.Errorf("ygg: %w", err)
		}
		if layer, ok := extRegistry().Get("ygg"); ok {
			_ = layer.Disable(svcName)
		}
		fmt.Printf("  %s  Yggdrasil: removed\n", styles.Warning.Render("→"))
	}

	// Reload Caddy
	if err := caddy.New(root).Reload(); err != nil {
		fmt.Printf("  %s  Caddy reload: %v\n", styles.Warning.Render("!"), err)
	}

	// Network extensions handle their own reload via layer.Disable()

	if disableStop {
		fmt.Printf("  %s  Stopping container…\n", styles.Muted.Render("→"))
		_ = stopAndRemoveService(root, svcName)
	}

	fmt.Println()
	return nil
}

// stopAndRemoveService stops and removes a service container via docker compose down.
func stopAndRemoveService(root, name string) error {
	return run.Default().DockerComposeEnv(
		run.ServiceComposeFile(root, name),
		buildEnv(root, name),
		"down",
	)
}
