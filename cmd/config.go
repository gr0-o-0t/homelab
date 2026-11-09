package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/swarm"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config <service>",
	Short: "Show a service's resolved compose configuration",
	Long: `Show a service's docker-compose.yml as homelab loads it to deploy on Swarm:
validated, with every ${VAR} interpolated from its config and secrets.

WARNING: the output contains resolved secrets (API tokens, passwords) in
plaintext — don't share it or redirect it anywhere you wouldn't put a secret.

  homelab config jellyfin`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, name := configDir(), args[0]
		if err := validateService(root, name); err != nil {
			return err
		}
		warnConfigContainsSecrets()
		p, err := swarm.LoadProject(filepath.Join(root, "services", name), buildEnv(root, name))
		if err != nil {
			return err
		}
		out, err := p.MarshalYAML()
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(out)
		return err
	},
}

// warnConfigContainsSecrets tells the user the output they're about to see
// has resolved secrets inlined in plaintext (every ${VAR} reference is
// interpolated, including keyring-sourced ones).
func warnConfigContainsSecrets() {
	fmt.Fprintln(os.Stderr, "warning: this output contains resolved secrets in plaintext — do not share it")
}
