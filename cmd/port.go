package cmd

import (
	"github.com/groot/homelab/internal/run"
	"github.com/spf13/cobra"
)

var portCmd = &cobra.Command{
	Use:   "port <service> <private-port>",
	Short: "Print the public port for a port binding",
	Long: `Print the public-facing port for a private port binding
(equivalent to 'docker compose port').

  homelab port jellyfin 8096    # show host port mapped to container port 8096`,
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		privatePort := args[1]
		root := configDir()

		if err := validateService(root, name); err != nil {
			return err
		}

		return run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name),
			buildEnv(root, name),
			"port", name, privatePort,
		)
	},
}
