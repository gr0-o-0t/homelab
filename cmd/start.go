package cmd

import (
	"github.com/groot/homelab/internal/desired"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start <service...>",
	Short: "Start services (on Swarm, the same as up)",
	Long: `Start services. Swarm keeps no stopped containers to resume, so start is
the same as 'homelab up'.

  homelab start jellyfin immich  # these services
  homelab start -a               # every installed service
  homelab start --group media    # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceStart,
}

var startFlags = batchFlags{}

func runServiceStart(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, "start", startFlags.all, startFlags.group, args)
	if err != nil {
		return err
	}
	// Swarm keeps no stopped containers to resume: start is up.
	return forEachService(root, names, func(name string) error {
		recordService(root, name, desired.Running)
		return upOne(root, name)
	})
}

func init() {
	startCmd.Flags().BoolVarP(&startFlags.all, "all", "a", false, "Start all installed services")
	startCmd.Flags().StringVar(&startFlags.group, "group", "", "Start a named service group")
	_ = startCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
