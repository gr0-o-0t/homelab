package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/network/layers"
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
	disableLayerFlags = map[string]*bool{} // layer name → its --<flag>
	disableAll        bool                 // -a: every layer, private included
	disableStop       bool                 // --stop: additionally take the service down
)

func init() {
	for _, l := range layers.Static() {
		if l.Flag() != "" {
			disableLayerFlags[l.Name()] = disableCmd.Flags().Bool(l.Flag(), false, "Remove "+l.Label()+" config")
		}
	}
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

	hasSpecific := false
	for _, on := range disableLayerFlags {
		hasSpecific = hasSpecific || *on
	}
	mgr, _, explain := quietCaddy(root)

	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Disable: %s", svcName)))

	// Without flags only the private tailnet route goes; -a takes every layer.
	// Each layer loses its Caddy blocks, then its own config (tunnel ingress,
	// hidden service, forwarder).
	for _, l := range extRegistry().All() {
		private := l.Flag() == ""
		switch {
		case disableAll:
		case private && hasSpecific, !private && !*disableLayerFlags[l.Name()]:
			continue
		}
		if err := routing.Disable(root, l, svcName); err != nil && !(private && disableAll) {
			return fmt.Errorf("%s: %w", l.Name(), err)
		}
		if private {
			fmt.Printf("  %s  Private: removed\n", styles.Warning.Render("→"))
		} else {
			fmt.Printf("  %s  %s: removed\n", styles.Warning.Render("→"), l.Label())
		}
	}

	if err := explain(mgr.Reload()); err != nil {
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
