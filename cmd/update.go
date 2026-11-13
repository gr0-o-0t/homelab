package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var serviceUpdateCmd = &cobra.Command{
	Use:   "update <service...>",
	Short: "Pull latest images and redeploy services",
	Long: `With no service, report whether a newer homelab release exists (this CLI's
and the leader agent's versions, then the latest GitHub release).

Pull the latest Docker images and redeploy services on Swarm, each tag
resolved to its current digest.

  homelab update jellyfin immich  # these services
  homelab update -a               # every installed service
  homelab update --group media    # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceUpdate,
}

var updateFlags = batchFlags{}

func runServiceUpdate(c *cobra.Command, args []string) error {
	if bareUpdate(c) && os.Getenv("HOMELAB_AGENT") != "1" {
		return runUpdateCheck(cmdContext(), c.OutOrStdout())
	}
	root := configDir()
	names, err := resolveTargets(root, "update", updateFlags.all, updateFlags.group, args)
	if err != nil {
		return err
	}
	return forEachService(root, names, func(name string) error {
		recordService(root, name, desired.Running)
		// Update pulls, resolves each tag to its current digest and removes
		// services no longer in the compose file. up never does the first
		// two (R53): this is the deliberate upgrade.
		fmt.Printf("%s Updating %s\n", styles.Primary.Render("→"), styles.Bold.Render(name))
		return deployOne(root, name, (*swarm.Deployer).Update)
	})
}

func init() {
	serviceUpdateCmd.Flags().BoolVarP(&updateFlags.all, "all", "a", false, "Update all installed services")
	serviceUpdateCmd.Flags().StringVar(&updateFlags.group, "group", "", "Update a named service group")
	_ = serviceUpdateCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
