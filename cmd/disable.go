package cmd

import (
	"errors"
	"fmt"
	"slices"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var disableCmd = &cobra.Command{
	Use:   "disable <service>",
	Short: "Disable network exposure for a service",
	Long: `Remove network exposure layers for a service.

Select which layers to remove with extension flags:

  --cf    remove Cloudflare Tunnel config

Tor, I2P and Yggdrasil (--tor, --i2p, --ygg) are not served in cluster mode;
the flags (and -a) still remove them from a service enabled on them earlier.

Without flags, only the private tailnet config is removed.
-a/--all removes every layer, private included. The container keeps running;
add --stop to also scale it to zero.

Examples:
  homelab disable gitea                  # private only
  homelab disable gitea --cf             # remove CF exposure
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
	disableUnavail    map[string]*bool     // flags of layers not registered in cluster mode
)

func init() {
	for _, l := range layers.Static() {
		if l.Flag() != "" {
			disableLayerFlags[l.Name()] = disableCmd.Flags().Bool(l.Flag(), false, "Remove "+l.Label()+" config")
		}
	}
	disableUnavail = addUnavailableFlags(disableCmd)
	disableCmd.Flags().BoolVarP(&disableAll, "all", "a", false, "Remove every layer, private included")
	disableCmd.Flags().BoolVar(&disableStop, "stop", false, "Also scale the service to zero")
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
	for _, on := range disableUnavail {
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
	// Layers cluster mode no longer serves (tor, i2p, ygg) can still be on in
	// an old exposure.yaml. Disabling them removes them from the state and
	// tears their daemon side down (best effort); enabling them stays refused.
	st, _ := exposure.Load(root, svcName)
	var unserved []network.NetworkLayer
	for _, l := range layers.Unavailable(root, run.Default(), func() map[string]string { return buildEnv(root, "") }) {
		if (disableAll || *disableUnavail[l.Name()]) && st.On(l.Name()) {
			ls = append(ls, l)
			unserved = append(unserved, l)
		}
	}

	// The public name is resolved before the exposure state goes: it may be a
	// --name that only the state remembers.
	env := buildEnv(root, "")
	publicHost := publicHostname(svcName, env)

	mgr, _, explain := quietCaddy(root)
	err := mgr.Disable(extRegistry(), svcName, ls)
	if errors.Is(err, caddy.ErrInvalidConfig) {
		return explain(err) // rolled back: nothing was disabled
	}
	for _, l := range ls {
		if l.Flag() == "" {
			fmt.Printf("  %s  Private: removed\n", styles.Warning.Render("→"))
			continue
		}
		fmt.Printf("  %s  %s: removed\n", styles.Warning.Render("→"), l.Label())
		if slices.Contains(unserved, l) {
			fmt.Printf("  %s  %s is not served in cluster mode; removed from the service's state only\n",
				styles.Warning.Render("!"), l.Name())
			continue
		}
		if l.Name() == "cf" && env["CLOUDFLARE_API_TOKEN"] != "" {
			// Best effort: only a tunnel CNAME is removed, and none may exist.
			if n, derr := deleteTunnelCNAME(cfAPIBase, env["CLOUDFLARE_API_TOKEN"], env["DOMAIN"], publicHost); derr == nil {
				fmt.Printf("  %s  DNS: %s removed (%d record(s))\n", styles.Warning.Render("→"), publicHost, n)
			}
		}
	}
	if err != nil {
		fmt.Printf("  %s  Caddy reload: %v\n", styles.Warning.Render("!"), explain(err))
	}

	if disableStop {
		recordService(root, svcName, desired.Stopped)
		fmt.Printf("  %s  Stopping container…\n", styles.Muted.Render("→"))
		if err := stopService(root, svcName); err != nil {
			return err
		}
	}

	fmt.Println()
	return nil
}

// stopService takes a service down: its Swarm services scale to zero.
func stopService(root, name string) error {
	return deploy(root, name, (*swarm.Deployer).Down)
}
