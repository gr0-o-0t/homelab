package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down [service]",
	Short: "Stop and remove containers (primary lifecycle)",
	Long: `Stop and remove service containers and networks (equivalent to 'docker compose down').

  homelab down              # core stack
  homelab down jellyfin     # one service
  homelab down --all        # every installed service
  homelab down --group media  # all services in the "media" group`,
	Args:              cobra.MaximumNArgs(1),
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
	downCmd.Flags().BoolVar(&downFlags.all, "all", false, "Stop all installed services")
	downCmd.Flags().StringVar(&downFlags.group, "group", "", "Stop a named service group")
	_ = downCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
