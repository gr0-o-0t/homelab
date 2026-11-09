package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop <service...>",
	Short: "Stop services (on Swarm, the same as down)",
	Long: `Stop services. Swarm keeps no stopped-but-kept containers, so stop is the
same as 'homelab down': scaled to zero, configuration and volumes kept.

  homelab stop jellyfin immich  # these services
  homelab stop -a               # every installed service
  homelab stop --group media    # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceStop,
}

var stopFlags = batchFlags{}

func runServiceStop(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, "stop", stopFlags.all, stopFlags.group, args)
	if err != nil {
		return err
	}
	return forEachService(root, names, func(name string) error {
		recordService(root, name, desired.Stopped)
		fmt.Printf("%s Stopping %s…\n", styles.Warning.Render("→"), styles.Bold.Render(name))
		// Swarm has no stopped-but-kept container: stop is down.
		return deploy(root, name, (*swarm.Deployer).Down)
	})
}

func init() {
	stopCmd.Flags().BoolVarP(&stopFlags.all, "all", "a", false, "Stop all installed services")
	stopCmd.Flags().StringVar(&stopFlags.group, "group", "", "Stop a named service group")
	_ = stopCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
