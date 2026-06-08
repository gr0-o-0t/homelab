package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var coreCmd = &cobra.Command{
	Use:   "core",
	Short: "Manage the core stack (Tailscale + Caddy + network extensions)",
}

var coreStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start Tailscale, Caddy, and enabled network extensions",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")
		fmt.Printf("%s Starting core stack…\n", styles.Primary.Render("→"))
		for _, note := range activeExtNotes(dir) {
			fmt.Printf("  %s\n", styles.Muted.Render(note))
		}
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, "up", "-d", "--build")...,
		)
	},
}

var coreStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop Tailscale, Caddy, and all network extensions",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")
		fmt.Printf("%s Stopping core stack…\n", styles.Warning.Render("→"))
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, "down")...,
		)
	},
}

var coreRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart Tailscale, Caddy, and all network extensions",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")
		fmt.Printf("%s Restarting core stack…\n", styles.Primary.Render("→"))
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, "restart")...,
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
			withProfiles(dir, "logs", "-f")...,
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
			withProfiles(dir, "ps")...,
		)
	},
}

func init() {
	coreCmd.AddCommand(coreStartCmd, coreStopCmd, coreRestartCmd, coreLogsCmd, coreStatusCmd)
}

// withProfiles prepends --profile <name> for each extension that is
// enabled in the root config, activating optional Docker Compose services.
func withProfiles(cfgDir string, args ...string) []string {
	cfgFile := config.RootConfigFile(cfgDir, rootFlags.configFile)
	cfg, _ := config.Load(cfgFile)

	var profiles []string
	if cfg != nil {
		for _, ext := range cfg.Extensions {
			profiles = append(profiles, "--profile", config.ExtensionProfile(ext))
		}
	}
	if len(profiles) == 0 {
		return args
	}
	return append(profiles, args...)
}

// activeExtNotes returns user-facing notes about which extensions will start.
func activeExtNotes(cfgDir string) []string {
	cfgFile := config.RootConfigFile(cfgDir, rootFlags.configFile)
	cfg, _ := config.Load(cfgFile)

	var notes []string
	if cfg != nil {
		for _, ext := range cfg.Extensions {
			notes = append(notes, fmt.Sprintf("%s enabled — starting %s",
				config.ExtensionLabel(ext), ext))
		}
	}
	return notes
}
