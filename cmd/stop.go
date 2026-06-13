package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop [service]",
	Short: "Stop service containers without removing them",
	Long: `Stop running service containers without removing them.
Use 'down' to stop and remove containers.

  homelab stop              # core stack (stop containers)
  homelab stop jellyfin     # one service

Note: This runs 'docker compose stop' under the hood — containers are
stopped but not removed. Use 'homelab down' to stop and remove.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()

		if len(args) > 0 {
			name := args[0]
			if err := validateService(dir, name); err != nil {
				return err
			}
			fmt.Printf("%s Stopping %s\n", styles.Warning.Render("→"), styles.Bold.Render(name))
			return run.Default().DockerComposeEnv(
				run.ServiceComposeFile(dir, name),
				buildEnv(dir, name),
				"stop",
			)
		}

		env := buildEnv(dir, "")
		fmt.Printf("%s Stopping core stack…\n", styles.Warning.Render("→"))
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, "stop")...,
		)
	},
}
