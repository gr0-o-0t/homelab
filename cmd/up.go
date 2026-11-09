package cmd

import (
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up <service...>",
	Short: "Deploy and start services on Swarm",
	Long: `Deploy services on Swarm and scale them up, shared databases first.

  homelab up jellyfin immich  # these services
  homelab up -a               # every installed service
  homelab up --group media    # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceUp,
}

var upFlags = batchFlags{}

func init() {
	upCmd.Flags().BoolVarP(&upFlags.all, "all", "a", false, "Start all installed services")
	upCmd.Flags().StringVar(&upFlags.group, "group", "", "Start a named service group")
	_ = upCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
