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
-a/--all removes every layer, private included. The container keeps running;
add --stop to also stop and remove it.

Examples:
  homelab disable gitea                  # private only
  homelab disable gitea --cf             # remove CF exposure
  homelab disable gitea --cf --i2p       # CF + I2P
  homelab disable gitea -a               # every layer
  homelab disable gitea -a --stop        # every layer, then down`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runDisable,
}

var (
	disableCf   bool
	disableI2P  bool
	disableTor  bool
	disableYgg  bool
	disableAll  bool // -a: every layer, private included
	disableStop bool // --stop: additionally take the service down
)

func init() {
	disableCmd.Flags().BoolVar(&disableCf, "cf", false, "Remove Cloudflare Tunnel config")
	disableCmd.Flags().BoolVar(&disableI2P, "i2p", false, "Remove I2P eepsite config")
	disableCmd.Flags().BoolVar(&disableTor, "tor", false, "Remove Tor onion service config")
	disableCmd.Flags().BoolVar(&disableYgg, "ygg", false, "Remove Yggdrasil mesh config")
	disableCmd.Flags().BoolVarP(&disableAll, "all", "a", false, "Remove every layer, private included")
	disableCmd.Flags().BoolVar(&disableStop, "stop", false, "Also stop and remove the service containers")
	rootCmd.AddCommand(disableCmd)
}

func runDisable(cmd *cobra.Command, args []string) error {
	svcName := args[0]
	root := configDir()
	// Only the name is checked: disable must still clean up the routes of a
	// service whose directory is already gone.
	if err := validName(svcName); err != nil {
		return err
	}

	selected := map[string]bool{
		"cf": disableCf || disableAll, "i2p": disableI2P || disableAll,
		"tor": disableTor || disableAll, "ygg": disableYgg || disableAll,
	}
	hasSpecific := disableCf || disableI2P || disableTor || disableYgg

	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Disable: %s", svcName)))

	// Private tailnet: the default target, and part of --all.
	if !hasSpecific || disableAll {
		if err := routing.DisablePrivate(root, svcName, nil); err != nil && !disableAll {
			return err
		}
		fmt.Printf("  %s  Private: removed\n", styles.Warning.Render("→"))
	}

	// Extension layers: drop the Caddy blocks, then the layer's own config
	// (tunnel ingress, hidden service, forwarder).
	for _, ext := range []string{"cf", "i2p", "tor", "ygg"} {
		if !selected[ext] {
			continue
		}
		if err := configgen.RemoveAllPortFiles(root, ext, svcName); err != nil {
			return fmt.Errorf("%s: %w", ext, err)
		}
		if layer, ok := extRegistry().Get(ext); ok {
			_ = layer.Disable(svcName)
		}
		fmt.Printf("  %s  %s: removed\n", styles.Warning.Render("→"), configgen.ExtensionLabel(ext))
	}

	if err := caddy.New(root).Reload(); err != nil {
		fmt.Printf("  %s  Caddy reload: %v\n", styles.Warning.Render("!"), err)
	}

	if disableStop {
		fmt.Printf("  %s  Stopping container…\n", styles.Muted.Render("→"))
		if err := stopAndRemoveService(root, svcName); err != nil {
			return err
		}
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
