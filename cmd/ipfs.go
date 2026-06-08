package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

const ipfsContainer = "ipfs"

var ipfsCmd = &cobra.Command{
	Use:   "ipfs",
	Short: "Manage IPFS Kubo node",
	Long:  "Inspect the IPFS node and manage the HTTP Gateway route.",
}

// ── status ────────────────────────────────────────────────────────────────────

var ipfsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show IPFS node status and peer count",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		env := buildEnv(root, "")

		fmt.Printf("\n%s\n\n", styles.Header.Render("IPFS Node"))

		if !extEnabled(root, "ipfs") {
			fmt.Printf("  %s  IPFS not enabled.\n", styles.Warning.Render("!"))
			fmt.Printf("  Run %s to enable.\n\n",
				styles.Primary.Render("homelab ext enable ipfs"))
			return nil
		}

		state := containerStatus(ipfsContainer)
		if state == "running" {
			fmt.Printf("  %s  ipfs  %s\n", styles.Success.Render("✓"), styles.StateTag(state))
		} else {
			fmt.Printf("  %s  ipfs  %s\n", styles.Err.Render("✗"), styles.StateTag(state))
			fmt.Printf("\n  Start with: %s\n\n", styles.Primary.Render("homelab core start"))
			return nil
		}

		// Peer count
		out, err := exec.Command(
			"docker", "exec", ipfsContainer,
			"ipfs", "swarm", "peers",
		).Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			count := 0
			for _, l := range lines {
				if l != "" {
					count++
				}
			}
			fmt.Printf("  %s  %s  peers\n",
				styles.Muted.Render("↳"),
				styles.Bold.Render(fmt.Sprintf("%d", count)))
		}

		// Gateway access
		mgr := caddy.New(root)
		enabled, _ := mgr.IsEnabled("ipfs")
		if enabled {
			fmt.Printf("  %s  Gateway:  %s\n",
				styles.Muted.Render("↳"),
				styles.Primary.Render(fmt.Sprintf(
					"https://ipfs.%s.%s",
					env["HOME_SUBDOMAIN"],
					env["DOMAIN"],
				)),
			)
		}

		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var ipfsLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Stream IPFS container logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		env := buildEnv(root, "")
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(root),
			env,
			withProfiles(root, "logs", "-f", ipfsContainer)...,
		)
	},
}

// ── gateway enable ────────────────────────────────────────────────────────────

var ipfsGatewayEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Expose IPFS Gateway via Caddy at ipfs.<home>.<domain>",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		// Create a caddy.conf for IPFS gateway
		svcDir := filepath.Join(root, "services", "ipfs")
		os.MkdirAll(svcDir, 0o755)

		caddyConf := `ipfs.{$HOME_SUBDOMAIN}.{$DOMAIN} {
    import wildcard_tls
    reverse_proxy ipfs:8080
}
`
		confPath := filepath.Join(svcDir, "caddy.conf")
		if err := os.WriteFile(confPath, []byte(caddyConf), 0o644); err != nil {
			return fmt.Errorf("writing caddy.conf: %w", err)
		}

		if err := runWithSpinner("Enabling IPFS gateway route…", func(r *run.Commander) error {
			return caddy.NewWithRunner(root, r).Enable("ipfs")
		}); err != nil {
			return fmt.Errorf("enabling route: %w", err)
		}

		env := buildEnv(root, "")
		fmt.Printf("%s IPFS Gateway at https://ipfs.%s.%s\n",
			styles.Success.Render("✓"),
			env["HOME_SUBDOMAIN"],
			env["DOMAIN"],
		)
		fmt.Println()
		return nil
	},
}

// ── gateway disable ───────────────────────────────────────────────────────────

var ipfsGatewayDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Remove IPFS Gateway from Caddy routing",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		if err := runWithSpinner("Disabling IPFS gateway route…", func(r *run.Commander) error {
			return caddy.NewWithRunner(root, r).Disable("ipfs")
		}); err != nil {
			return fmt.Errorf("disabling route: %w", err)
		}
		fmt.Printf("%s IPFS Gateway removed from routing\n\n", styles.Success.Render("✓"))
		return nil
	},
}

// ── gateway ───────────────────────────────────────────────────────────────────

var ipfsGatewayCmd = &cobra.Command{
	Use:   "gateway",
	Short: "Manage IPFS Gateway Caddy route",
}

func init() {
	ipfsGatewayCmd.AddCommand(ipfsGatewayEnableCmd, ipfsGatewayDisableCmd)
	ipfsCmd.AddCommand(ipfsStatusCmd, ipfsLogsCmd, ipfsGatewayCmd)
	rootCmd.AddCommand(ipfsCmd)
}
