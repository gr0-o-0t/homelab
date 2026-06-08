package cmd

import (
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

const cloudflaredContainer = "cloudflared"

var tunnelCmd = &cobra.Command{
	Use:   "tunnel",
	Short: "Manage Cloudflare Tunnel (cloudflared)",
	Long:  "Inspect cloudflared and manage DNS routes for public-internet service exposure.",
}

// ── status ────────────────────────────────────────────────────────────────────

var tunnelStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Cloudflare Tunnel status and connections",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		env := buildEnv(root, "")

		fmt.Printf("\n%s\n\n", styles.Header.Render("Cloudflare Tunnel"))

		if !extEnabled(root, "cf") {
			fmt.Printf("  %s  Cloudflare Tunnel not enabled.\n", styles.Warning.Render("!"))
			fmt.Printf("  Run %s to enable.\n\n",
				styles.Primary.Render("homelab ext enable cf"))
			return nil
		}

		if env["CF_TUNNEL_TOKEN"] == "" {
			fmt.Printf("  %s  CF_TUNNEL_TOKEN not configured.\n", styles.Warning.Render("!"))
			fmt.Printf("\n  Run %s to provide credentials.\n\n",
				styles.Primary.Render("homelab setup"))
			return nil
		}

		state := containerStatus(cloudflaredContainer)
		if state == "running" {
			fmt.Printf("  %s  cloudflared  %s\n", styles.Success.Render("✓"), styles.StateTag(state))
		} else {
			fmt.Printf("  %s  cloudflared  %s\n", styles.Err.Render("✗"), styles.StateTag(state))
			fmt.Printf("\n  Start with: %s\n\n", styles.Primary.Render("homelab core start"))
			return nil
		}

		if tunnelName := env["CF_TUNNEL_NAME"]; tunnelName != "" {
			fmt.Printf("  %s  tunnel name  %s\n", styles.Muted.Render("↳"), styles.Bold.Render(tunnelName))
		}

		fmt.Printf("\n  %s\n", styles.Bold.Render("Active connections"))
		if err := run.Default().DockerExec(cloudflaredContainer, "cloudflared", "tunnel", "info"); err != nil {
			fmt.Printf("  %s  (run %s for details)\n",
				styles.Muted.Render("!"), styles.Primary.Render("homelab tunnel logs"))
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var tunnelLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Stream cloudflared logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		env := buildEnv(root, "")
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(root),
			env,
			withProfiles(root, "logs", "-f", "cloudflared")...,
		)
	},
}

// ── route ─────────────────────────────────────────────────────────────────────

var tunnelRouteCmd = &cobra.Command{
	Use:   "route",
	Short: "Manage Cloudflare DNS routes",
}

var tunnelRouteAddCmd = &cobra.Command{
	Use:   "add <service>",
	Short: "Add a Cloudflare DNS CNAME route for a service",
	Long: `Register a DNS CNAME so Cloudflare Tunnel serves the service publicly.

Requires CF_TUNNEL_NAME to be set (run 'homelab setup') and the cloudflared
container to be running ('homelab core start').

After adding the DNS route, enable the public Caddy config:
  homelab service enable <service> --public`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		root := configDir()
		env := buildEnv(root, "")
		if !extEnabled(root, "cf") {
			return fmt.Errorf("Cloudflare Tunnel not enabled\n\n  Run %s to enable",
				styles.Primary.Render("homelab ext enable cf"))
		}
		if err := requireTunnelConfig(env); err != nil {
			return err
		}
		hostname := publicHostname(name, env)
		tunnelName := env["CF_TUNNEL_NAME"]
		fmt.Printf("%s Adding DNS route: %s → tunnel/%s\n",
			styles.Primary.Render("→"), styles.Bold.Render(hostname), tunnelName)
		if err := run.Default().DockerExec(cloudflaredContainer,
			"cloudflared", "tunnel", "route", "dns", tunnelName, hostname,
		); err != nil {
			return fmt.Errorf(
				"adding route %s: %w\n\n  Ensure CLOUDFLARE_API_TOKEN has DNS:Edit permissions", hostname, err)
		}
		fmt.Printf("%s DNS route added: %s\n", styles.Success.Render("✓"), styles.Bold.Render(hostname))
		fmt.Printf("  Enable public routing: %s\n\n",
			styles.Primary.Render(fmt.Sprintf("homelab service enable %s --public", name)))
		return nil
	},
}

var tunnelRouteRmCmd = &cobra.Command{
	Use:               "rm <service>",
	Short:             "Remove a Cloudflare DNS route for a service",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		root := configDir()
		env := buildEnv(root, "")
		if err := requireTunnelConfig(env); err != nil {
			return err
		}
		hostname := publicHostname(name, env)
		fmt.Printf("%s Removing DNS route: %s\n", styles.Warning.Render("→"), styles.Bold.Render(hostname))
		// cloudflared doesn't expose a direct "delete DNS record" sub-command;
		// overwriting with an empty tunnel name is the closest workaround.
		// Instruct the user to remove the CNAME from the Cloudflare dashboard if this fails.
		if err := run.Default().DockerExec(cloudflaredContainer,
			"cloudflared", "tunnel", "route", "dns", "--overwrite-dns",
			env["CF_TUNNEL_NAME"], hostname,
		); err != nil {
			fmt.Printf("  %s  Auto-remove failed. Delete the CNAME record for %s manually:\n",
				styles.Warning.Render("!"), styles.Bold.Render(hostname))
			fmt.Printf("  %s\n\n",
				styles.Muted.Render("  → Cloudflare Dashboard → DNS → delete CNAME pointing to your tunnel"))
			return nil
		}
		fmt.Printf("%s DNS route removed: %s\n\n", styles.Success.Render("✓"), styles.Bold.Render(hostname))
		return nil
	},
}

// ── helpers ───────────────────────────────────────────────────────────────────

func requireTunnelConfig(env map[string]string) error {
	var missing []string
	if env["CF_TUNNEL_TOKEN"] == "" {
		missing = append(missing, "CF_TUNNEL_TOKEN")
	}
	if env["CF_TUNNEL_NAME"] == "" {
		missing = append(missing, "CF_TUNNEL_NAME")
	}
	if len(missing) > 0 {
		return fmt.Errorf("cloudflare Tunnel not fully configured: %s missing\n\n  Run %s",
			strings.Join(missing, ", "), styles.Primary.Render("homelab setup"))
	}
	return nil
}

// publicHostname returns the public FQDN for a service (e.g. jellyfin.pub.example.com).
func publicHostname(svcName string, env map[string]string) string {
	pubSub := env["PUB_SUBDOMAIN"]
	if pubSub == "" {
		pubSub = "pub"
	}
	return fmt.Sprintf("%s.%s.%s", svcName, pubSub, env["DOMAIN"])
}

func init() {
	tunnelRouteCmd.AddCommand(tunnelRouteAddCmd, tunnelRouteRmCmd)
	tunnelCmd.AddCommand(tunnelStatusCmd, tunnelLogsCmd, tunnelRouteCmd)
	rootCmd.AddCommand(tunnelCmd)
}
