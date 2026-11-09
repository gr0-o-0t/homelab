package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var serviceUpdateCmd = &cobra.Command{
	Use:   "update <service...>",
	Short: "Pull latest images and redeploy services",
	Long: `Pull the latest Docker images and redeploy services on Swarm, each tag
resolved to its current digest.

  homelab update jellyfin immich  # these services
  homelab update -a               # every installed service
  homelab update --group media    # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceUpdate,
}

var updateFlags = batchFlags{}

func runServiceUpdate(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, "update", updateFlags.all, updateFlags.group, args)
	if err != nil {
		return err
	}
	return forEachService(root, names, func(name string) error {
		recordService(root, name, desired.Running)
		// Up pulls, resolves each tag to its current digest and removes
		// services no longer in the compose file: it is the update.
		fmt.Printf("%s Updating %s\n", styles.Primary.Render("→"), styles.Bold.Render(name))
		return upOne(root, name)
	})
}

func init() {
	serviceUpdateCmd.Flags().BoolVarP(&updateFlags.all, "all", "a", false, "Update all installed services")
	serviceUpdateCmd.Flags().StringVar(&updateFlags.group, "group", "", "Update a named service group")
	_ = serviceUpdateCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
