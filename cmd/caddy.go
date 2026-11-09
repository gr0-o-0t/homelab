package cmd

import (
	"bytes"
	"fmt"
	"io"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/edge"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/spinner"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

// Caddy runs inside the leader agent (internal/edge): there is no Caddy
// container. `caddy status` asks its admin socket, `caddy logs` reads the
// agent container's logs, which carry Caddy's.

var caddyCmd = &cobra.Command{
	Use:   "caddy",
	Short: "Inspect the agent's Caddy reverse proxy",
	Long: `Inspect the Caddy that runs inside the leader agent.

Use "homelab reload"/"homelab validate" to manage its configuration —
this command group is read-only status/logs.`,
}

var caddyStatusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"ps"},
	Short:   "Show whether the agent's Caddy answers on its admin socket",
	Args:    cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		fmt.Printf("\n%s\n\n", styles.Header.Render("Caddy"))
		sock := edge.AdminSocket(configDir())
		if err := edge.Reachable(sock); err != nil {
			fmt.Printf("  %s  caddy  %s\n", styles.Err.Render("✗"), styles.StateTag("not running"))
			fmt.Printf("\n  %s\n\n", styles.Muted.Render(err.Error()))
			return nil
		}
		fmt.Printf("  %s  caddy  %s  %s\n\n", styles.Success.Render("✓"), styles.StateTag(containerStateRunning),
			styles.Muted.Render("admin "+sock))
		return nil
	},
}

var caddyLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Stream the agent's logs, which carry Caddy's",
	Args:  cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		return agentLogs(c.OutOrStdout(), c.ErrOrStderr(), []string{"--follow"})
	},
}

func init() {
	caddyCmd.AddCommand(caddyStatusCmd)
	caddyCmd.AddCommand(caddyLogsCmd)
	rootCmd.AddCommand(caddyCmd)
}

// agentLogs prints the leader agent container's logs: Caddy, the tailnet
// node and the agent itself log there. flags are `docker logs` flags.
func agentLogs(stdout, stderr io.Writer, flags []string) error {
	args := append(append([]string{"logs"}, flags...), bootstrap.AgentContainer)
	return serviceLogsCLI(cmdContext(), args, stdout, stderr)
}

var reloadCmd = &cobra.Command{
	Use:   "reload [service]",
	Short: "Reload Caddy config or a service's routing",
	Long: `Reload configuration changes.

Without arguments, assembles every service's routes and loads them into the
agent's Caddy (validated by the load: a rejected config leaves the running one
in place). With a service name, re-renders the service's Caddy file
(caddy/sites/<service>.conf) from its exposure.yaml — every layer it is
enabled on, keeping its --name and --ports — and reloads Caddy once: picks up
edits to its config.yaml ports or caddy.routes.conf without redeploying.`,
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
	Short: "Check the Caddy config adapts, without loading it",
	Long: `Assemble the agent's Caddy config from every service's routes and adapt it,
as a reload would, without loading it into the running Caddy.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		if err := runWithSpinner("Validating the Caddy config…", func(r *run.Commander) error {
			return caddy.NewWithRunner(dir, r).Validate()
		}); err != nil {
			return err
		}
		fmt.Printf("%s Caddy config is valid\n", styles.Success.Render("✓"))
		return nil
	},
}

func runServiceReload(root, name string) error {
	if err := validateService(root, name); err != nil {
		return err
	}
	if err := requireMigrated(root, name); err != nil {
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
