package cmd

import (
	"errors"
	"fmt"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
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

	if err := requireMigrated(root, svcName); err != nil {
		return err
	}

	hasSpecific := false
	for _, on := range disableLayerFlags {
		hasSpecific = hasSpecific || *on
	}

	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Disable: %s", svcName)))

	// Without flags only the private tailnet route goes; -a takes every layer.
	// Each layer leaves the service's sites file, then loses its own config
	// (hidden service, tunnel, forwarder).
	var ls []network.NetworkLayer
	for _, l := range extRegistry().All() {
		private := l.Flag() == ""
		switch {
		case disableAll:
		case private && hasSpecific, !private && !*disableLayerFlags[l.Name()]:
			continue
		}
		ls = append(ls, l)
	}

	mgr, _, explain := quietCaddy(root)
	err := mgr.Disable(extRegistry(), svcName, ls)
	if errors.Is(err, caddy.ErrInvalidConfig) {
		return explain(err) // rolled back: nothing was disabled
	}
	for _, l := range ls {
		if l.Flag() == "" {
			fmt.Printf("  %s  Private: removed\n", styles.Warning.Render("→"))
		} else {
			fmt.Printf("  %s  %s: removed\n", styles.Warning.Render("→"), l.Label())
		}
	}
	if err != nil {
		fmt.Printf("  %s  Caddy reload: %v\n", styles.Warning.Render("!"), explain(err))
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
