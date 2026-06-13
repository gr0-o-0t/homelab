package cmd

import (
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config [service]",
	Short: "Show resolved Docker Compose configuration",
	Long: `Show the resolved Docker Compose configuration for a service or core stack.
This validates the compose file and displays the canonical config
(equivalent to 'docker compose config').

  homelab config              # core stack compose config
  homelab config jellyfin     # one service compose config`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		if len(args) > 0 {
			name := args[0]
			if err := validateService(root, name); err != nil {
				return err
			}
			return run.Default().DockerComposeEnv(
				run.ServiceComposeFile(root, name),
				buildEnv(root, name),
				"config",
			)
		}

		composeFile := run.CoreComposeFile(root)
		if _, err := os.Stat(composeFile); err != nil {
			return err
		}
		return run.Default().DockerComposeEnv(
			composeFile,
			buildEnv(root, ""),
			"config",
		)
	},
}
