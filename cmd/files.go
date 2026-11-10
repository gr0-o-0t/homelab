package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/swarm"
)

// filesCmd is the leader-side half of `homelab edit`: it reads and writes a
// service's compose file, config.yaml and bind-mounted config files in the
// state dir. Hidden plumbing, forwarded to the agent like any command.
var filesCmd = &cobra.Command{
	Use:    "files",
	Short:  "Read and write a service's editable files (used by edit)",
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
	Short:  "Replace an editable file of the service from stdin (validated) and redeploy it",
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
		root, svc, rel := configDir(), args[0], args[1]
		// The secrets manager is required: validation checks no secret value
		// ends up in a spec.
		if err := withDeployer(root, true, func(_ context.Context, d *swarm.Deployer) error {
			return d.WriteConfigFile(svc, rel, data)
		}); err != nil {
			return err
		}
		return applyEdit(root, svc, rel, c.ErrOrStderr())
	},
}

func init() {
	filesCmd.AddCommand(filesGetCmd, filesPutCmd)
	rootCmd.AddCommand(filesCmd)
}

// applyEdit rolls a saved file out the way `homelab up <svc>` does — shared
// databases and generated secrets prepared first — when the service is
// desired running. A stopped (or never started) service is left so: the
// file waits for the next `homelab up`.
func applyEdit(root, svc, rel string, w io.Writer) error {
	d, _, err := desired.LoadDesired(root)
	if err != nil {
		return err
	}
	if st := d.Services[svc]; st != desired.Running {
		state := "not running"
		if st == desired.Stopped {
			state = "stopped"
		}
		_, _ = fmt.Fprintf(w, "%s/%s saved; %s is %s — run `homelab up %s` to apply\n", svc, rel, svc, state, svc)
		return nil
	}
	recordService(root, svc, desired.Running)
	if err := upOne(root, svc); err != nil {
		return fmt.Errorf("saved %s, but redeploying %s failed: %w — the file is saved; run `homelab up %s` to retry", rel, svc, err, svc)
	}
	_, _ = fmt.Fprintf(w, "%s/%s saved and %s redeployed\n", svc, rel, svc)
	return nil
}
