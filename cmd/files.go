package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/swarm"
)

// filesCmd is the leader-side half of `homelab edit`: it reads and writes a
// service's bind-mounted config files in the state dir. Hidden plumbing,
// forwarded to the agent like any command.
var filesCmd = &cobra.Command{
	Use:    "files",
	Short:  "Read and write a service's bind-mounted config files (used by edit)",
	Hidden: true,
}

var filesGetCmd = &cobra.Command{
	Use:    "get <service> <path>",
	Short:  "Print a config file of the service (path relative to its directory)",
	Hidden: true,
	Args:   cobra.ExactArgs(2),
	RunE: func(c *cobra.Command, args []string) error {
		if err := validName(args[0]); err != nil {
			return err
		}
		// Read-only: no Docker, no secrets.
		d := &swarm.Deployer{Root: configDir(), ConfigFile: rootFlags.configFile}
		data, err := d.ReadConfigFile(args[0], args[1])
		if err != nil {
			return err
		}
		_, err = c.OutOrStdout().Write(data)
		return err
	},
}

var filesPutCmd = &cobra.Command{
	Use:    "put <service> <path>",
	Short:  "Replace a config file of the service from stdin and redeploy it",
	Hidden: true,
	Args:   cobra.ExactArgs(2),
	RunE: func(c *cobra.Command, args []string) error {
		if err := validName(args[0]); err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(c.InOrStdin(), swarm.MaxConfigSize+1))
		if err != nil {
			return err
		}
		return withDeployer(configDir(), true, func(ctx context.Context, d *swarm.Deployer) error {
			if err := d.WriteConfigFile(ctx, args[0], args[1], data); err != nil {
				return err
			}
			fmt.Fprintf(c.ErrOrStderr(), "%s/%s updated and %s redeployed\n", args[0], args[1], args[0])
			return nil
		})
	},
}

func init() {
	filesCmd.AddCommand(filesGetCmd, filesPutCmd)
	rootCmd.AddCommand(filesCmd)
}
