package cmd

import (
	"bytes"
	"fmt"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/spinner"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var caddyCmd = &cobra.Command{
	Use:   "caddy",
	Short: "Manage Caddy configuration",
}

var caddyReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Validate and gracefully reload Caddy",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := runWithSpinner("Reloading Caddy…", func(r *run.Commander) error {
			return caddy.NewWithRunner(configDir(), r).Reload()
		}); err != nil {
			return err
		}
		fmt.Printf("%s Caddy reloaded\n", styles.Success.Render("✓"))
		return nil
	},
}

var caddyValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate Caddyfile syntax without reloading",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := runWithSpinner("Validating Caddyfile…", func(r *run.Commander) error {
			return caddy.NewWithRunner(configDir(), r).Validate()
		}); err != nil {
			return err
		}
		fmt.Printf("%s Caddyfile is valid\n", styles.Success.Render("✓"))
		return nil
	},
}

func init() {
	caddyCmd.AddCommand(caddyReloadCmd, caddyValidateCmd)
}

// runWithSpinner runs fn while showing a spinner, with all Commander output
// captured in a buffer. The buffer is flushed to stdout only on error.
func runWithSpinner(msg string, fn func(r *run.Commander) error) error {
	var buf bytes.Buffer
	r := &run.Commander{Stdout: &buf, Stderr: &buf}
	if err := spinner.Run(msg, func() error { return fn(r) }); err != nil {
		if buf.Len() > 0 {
			fmt.Print(buf.String())
		}
		return err
	}
	return nil
}
