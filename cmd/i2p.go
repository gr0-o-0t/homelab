package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

const i2pContainer = "i2p"

var i2pCmd = &cobra.Command{
	Use:   "i2p",
	Short: "Manage I2P router and eepsite proxy",
	Long:  "Inspect the I2P router and manage eepsite tunnels.",
}

// ── status ────────────────────────────────────────────────────────────────────

var i2pStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show I2P router status",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		fmt.Printf("\n%s\n\n", styles.Header.Render("I2P Router"))

		if !extEnabled(root, "i2p") {
			fmt.Printf("  %s  I2P not enabled.\n", styles.Warning.Render("!"))
			fmt.Printf("  Run %s to enable.\n\n",
				styles.Primary.Render("homelab ext enable i2p"))
			return nil
		}

		state := containerStatus(i2pContainer)
		if state == "running" {
			fmt.Printf("  %s  i2p  %s\n", styles.Success.Render("✓"), styles.StateTag(state))
			fmt.Printf("\n  %s  Router Console: %s\n",
				styles.Muted.Render("↳"),
				styles.Primary.Render("http://i2p:7657"))
		} else {
			fmt.Printf("  %s  i2p  %s\n", styles.Err.Render("✗"), styles.StateTag(state))
			fmt.Printf("\n  Start with: %s\n\n", styles.Primary.Render("homelab core start"))
			return nil
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var i2pLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Stream I2P container logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		env := buildEnv(root, "")
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(root),
			env,
			withProfiles(root, "logs", "-f", i2pContainer)...,
		)
	},
}

// ── enable ────────────────────────────────────────────────────────────────────

var i2pEnablePort string

var i2pEnableCmd = &cobra.Command{
	Use:   "enable <service>",
	Short: "Create an eepsite tunnel for a service",
	Long: `Create an I2P eepsite tunnel that proxies to a service.

I2P eepsites are managed via the Router Console at http://i2p:7657.
This command provides a convenient shortcut by printing the tunnel details
needed to configure it through the web interface.

Full eepsite automation requires the SAM bridge to be enabled in the Router
Console and additional configuration. For now, use the Router Console:
  1. Open http://i2p:7657
  2. Navigate to Hidden Services Manager
  3. Create a new HTTP tunnel → <service>:<port>`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		root := configDir()

		port, _ := cmd.Flags().GetString("port")
		if port == "" {
			var err error
			port, err = detectServicePort(root, name)
			if err != nil {
				return fmt.Errorf("detecting port for %s: %w\n  Use --port to specify explicitly", name, err)
			}
		}

		fmt.Printf("\n%s\n\n", styles.Header.Render("I2P Eepsite: "+name))
		fmt.Printf("  %s  Tunnel:  %s → %s:%s\n",
			styles.Primary.Render("→"),
			styles.Bold.Render(name+".i2p"),
			name, port)
		fmt.Printf("\n  Configure in Router Console:\n")
		fmt.Printf("  %s  http://i2p:7657 → Hidden Services Manager\n", styles.Muted.Render("1."))
		fmt.Printf("  %s  Create new HTTP tunnel named %q\n", styles.Muted.Render("2."), name)
		fmt.Printf("  %s  Target: %s:%s\n", styles.Muted.Render("3."), name, port)
		fmt.Printf("  %s  Start the tunnel\n\n", styles.Muted.Render("4."))
		return nil
	},
}

// ── disable ───────────────────────────────────────────────────────────────────

var i2pDisableCmd = &cobra.Command{
	Use:               "disable <service>",
	Short:             "Remove an eepsite tunnel (Router Console required)",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		fmt.Printf("  %s  To remove eepsite for %s:\n", styles.Warning.Render("→"), styles.Bold.Render(name))
		fmt.Printf("  %s  1. Open http://i2p:7657\n", styles.Muted.Render(" "))
		fmt.Printf("  %s  2. Navigate to Hidden Services Manager\n", styles.Muted.Render(" "))
		fmt.Printf("  %s  3. Stop and delete the %q tunnel\n\n", styles.Muted.Render(" "), name)
		return nil
	},
}

func init() {
	i2pEnableCmd.Flags().StringVar(&i2pEnablePort, "port", "", "Override service port")
	i2pCmd.AddCommand(i2pStatusCmd, i2pLogsCmd, i2pEnableCmd, i2pDisableCmd)
	rootCmd.AddCommand(i2pCmd)
}
