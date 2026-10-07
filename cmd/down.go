package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down [service...]",
	Short: "Stop and remove containers; exposure is kept (no service: the core stack)",
	Long: `Stop and remove service containers and networks (equivalent to 'docker compose down').
Network exposure is configuration and is kept: 'up' brings the service back
exactly as it was. Use 'homelab disable' to remove exposure.

  homelab down              # core stack
  homelab down jellyfin immich  # these services
  homelab down -a           # every installed service
  homelab down --group media  # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		if len(args) > 0 || downFlags.all || downFlags.group != "" {
			return runServiceDown(cmd, args)
		}
		env := buildEnv(dir, "")
		fmt.Printf("%s Stopping core stack…\n", styles.Warning.Render("→"))
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, "down")...,
		)
	},
}

var downFlags = batchFlags{}

func init() {
	downCmd.Flags().BoolVarP(&downFlags.all, "all", "a", false, "Stop all installed services")
	downCmd.Flags().StringVar(&downFlags.group, "group", "", "Stop a named service group")
	_ = downCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
