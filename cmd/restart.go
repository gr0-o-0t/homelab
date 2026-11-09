package cmd

import (
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart <service...>",
	Short: "Restart services (force-update their Swarm services)",
	Long: `Restart services: every Swarm service of the stack is force-updated, which
replaces its tasks.

  homelab restart jellyfin immich  # these services
  homelab restart -a               # every installed service
  homelab restart --group media    # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceRestart,
}

var restartFlags = batchFlags{}

func init() {
	restartCmd.Flags().BoolVarP(&restartFlags.all, "all", "a", false, "Restart all installed services")
	restartCmd.Flags().StringVar(&restartFlags.group, "group", "", "Restart a named service group")
	_ = restartCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
