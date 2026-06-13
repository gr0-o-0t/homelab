package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start [service]",
	Short: "Start existing service containers",
	Long: `Start existing service containers without creating them first.
Use 'up' to create and start containers.

  homelab start              # core stack (start existing containers)
  homelab start jellyfin     # one service

Note: This runs 'docker compose start' under the hood — containers must
already exist. Use 'homelab up' to create and start.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()

		if len(args) > 0 {
			name := args[0]
			if err := validateService(dir, name); err != nil {
				return err
			}
			fmt.Printf("%s Starting %s\n", styles.Primary.Render("→"), styles.Bold.Render(name))
			return run.Default().DockerComposeEnv(
				run.ServiceComposeFile(dir, name),
				buildEnv(dir, name),
				"start",
			)
		}

		env := buildEnv(dir, "")
		composeFile := run.CoreComposeFile(dir)
		if _, err := os.Stat(composeFile); err != nil {
			return err
		}
		fmt.Printf("%s Starting core stack…\n", styles.Primary.Render("→"))
		return run.Default().DockerComposeEnv(
			composeFile,
			env,
			withProfiles(dir, "start")...,
		)
	},
}
