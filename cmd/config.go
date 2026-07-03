package cmd

import (
	"fmt"
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

WARNING: the output contains resolved secrets (API tokens, passwords) in
plaintext — don't share it or redirect it anywhere you wouldn't put a secret.

  homelab config              # core stack compose config
  homelab config jellyfin     # one service compose config`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		warnConfigContainsSecrets()

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

// warnConfigContainsSecrets tells the user the output they're about to see
// has resolved secrets inlined in plaintext (docker compose config fully
// interpolates ${VAR} references, including keyring-sourced ones).
func warnConfigContainsSecrets() {
	fmt.Fprintln(os.Stderr, "warning: this output contains resolved secrets in plaintext — do not share it")
}
