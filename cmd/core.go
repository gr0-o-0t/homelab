package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var coreCmd = &cobra.Command{
	Use:   "core",
	Short: "Manage the core stack (Tailscale + Caddy [+ cloudflared])",
}

var coreStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start Tailscale, Caddy, and optionally cloudflared",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")
		fmt.Printf("%s Starting core stack…\n", styles.Primary.Render("→"))
		if env["CF_TUNNEL_TOKEN"] != "" {
			fmt.Printf("  %s\n", styles.Muted.Render("Cloudflare Tunnel token detected — starting cloudflared"))
		}
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withTunnelProfile(env, "up", "-d", "--build")...,
		)
	},
}

var coreStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop Tailscale, Caddy, and cloudflared",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")
		fmt.Printf("%s Stopping core stack…\n", styles.Warning.Render("→"))
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withTunnelProfile(env, "down")...,
		)
	},
}

var coreRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart Tailscale, Caddy, and cloudflared",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")
		fmt.Printf("%s Restarting core stack…\n", styles.Primary.Render("→"))
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withTunnelProfile(env, "restart")...,
		)
	},
}

var coreLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail core stack logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withTunnelProfile(env, "logs", "-f")...,
		)
	},
}

var coreStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show core container status",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withTunnelProfile(env, "ps")...,
		)
	},
}

func init() {
	coreCmd.AddCommand(coreStartCmd, coreStopCmd, coreRestartCmd, coreLogsCmd, coreStatusCmd)
}

// withTunnelProfile prepends --profile tunnel to args when CF_TUNNEL_TOKEN is
// present in env, activating the optional cloudflared service.
func withTunnelProfile(env map[string]string, args ...string) []string {
	if env["CF_TUNNEL_TOKEN"] != "" {
		return append([]string{"--profile", "tunnel"}, args...)
	}
	return args
}
