package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/spinner"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

const caddyContainerName = "caddy"

var caddyCmd = &cobra.Command{
	Use:   caddyContainerName,
	Short: "Inspect the Caddy reverse proxy",
	Long: `Inspect the core Caddy container individually.

Use "homelab reload"/"homelab validate" to manage its configuration —
this command group is read-only status/logs.`,
}

var caddyStatusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"ps"},
	Short:   "Show Caddy container status",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("\n%s\n\n", styles.Header.Render("Caddy"))
		state := containerStatus(caddyContainerName)
		if state == containerStateRunning {
			fmt.Printf("  %s  caddy  %s\n", styles.Success.Render("✓"), styles.StateTag(state))
		} else {
			fmt.Printf("  %s  caddy  %s\n", styles.Err.Render("✗"), styles.StateTag(state))
			fmt.Printf("\n  Start with: %s\n", styles.Primary.Render("homelab start"))
		}
		fmt.Println()
		return nil
	},
}

var caddyLogsCmd = containerLogsCmd(caddyContainerName,
	"Stream Caddy container logs")

func init() {
	caddyCmd.AddCommand(caddyStatusCmd)
	caddyCmd.AddCommand(caddyLogsCmd)
	rootCmd.AddCommand(caddyCmd)
}

var reloadCmd = &cobra.Command{
	Use:   "reload [service]",
	Short: "Reload Caddy config or a service's routing",
	Long: `Reload configuration changes.

Without arguments, reloads the Caddy config (validate + graceful reload).
With a service name, re-links the service's Caddy config files and reloads
Caddy — picks up edits to caddy.conf or caddy.cf.conf without redeploying.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		if len(args) > 0 {
			return runServiceReload(dir, args[0])
		}
		if err := runWithSpinner("Reloading Caddy…", func(r *run.Commander) error {
			return caddy.NewWithRunner(dir, r).Reload()
		}); err != nil {
			return err
		}
		fmt.Printf("%s Caddy reloaded\n", styles.Success.Render("✓"))
		return nil
	},
}

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate Caddyfile syntax without reloading",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		caddyfilePath := filepath.Join(dir, "caddy", "Caddyfile")
		if _, err := os.Stat(caddyfilePath); os.IsNotExist(err) {
			return fmt.Errorf("caddyfile not found at %s: run 'homelab setup' first", caddyfilePath)
		}
		if err := runWithSpinner("Validating Caddyfile…", func(r *run.Commander) error {
			return caddy.NewWithRunner(dir, r).Validate()
		}); err != nil {
			return err
		}
		fmt.Printf("%s Caddyfile is valid\n", styles.Success.Render("✓"))
		return nil
	},
}

func runServiceReload(root, name string) error {
	if err := validateService(root, name); err != nil {
		return err
	}
	if err := runWithSpinner(
		fmt.Sprintf("Reloading %s config…", name),
		func(r *run.Commander) error {
			return caddy.NewWithRunner(root, r).ReloadService(name)
		},
	); err != nil {
		return err
	}
	fmt.Printf("%s %s config reloaded\n", styles.Success.Render("✓"), styles.Bold.Render(name))
	return nil
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
