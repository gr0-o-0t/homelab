package cmd

import (
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down <service...>",
	Short: "Stop services (scaled to zero); exposure is kept",
	Long: `Stop services: their Swarm services scale to zero, keeping their
configuration and volumes. Network exposure is configuration and is kept: 'up'
brings the service back exactly as it was. Use 'homelab disable' to remove
exposure.

  homelab down jellyfin immich  # these services
  homelab down -a               # every installed service
  homelab down --group media    # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceDown,
}

var downFlags = batchFlags{}

func init() {
	downCmd.Flags().BoolVarP(&downFlags.all, "all", "a", false, "Stop all installed services")
	downCmd.Flags().StringVar(&downFlags.group, "group", "", "Stop a named service group")
	_ = downCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
