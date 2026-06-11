package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

const yggContainer = "yggdrasil"

var yggCmd = &cobra.Command{
	Use:     "ygg",
	Aliases: []string{yggContainer},
	Short:   "Manage Yggdrasil IPv6 mesh node",
	Long:    "Inspect the Yggdrasil mesh node and manage per-service socat port forwarders.",
}

// ── status ────────────────────────────────────────────────────────────────────

var yggStatusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"ps"},
	Short:   "Show Yggdrasil node and forwarding status",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		fmt.Printf("\n%s\n\n", styles.Header.Render("Yggdrasil Mesh Node"))

		if !extEnabled(root, "yggdrasil") {
			fmt.Printf("  %s  Yggdrasil not enabled.\n", styles.Warning.Render("!"))
			fmt.Printf("  Run %s to enable.\n\n",
				styles.Primary.Render("homelab ext enable yggdrasil"))
			return nil
		}

		state := containerStatus(yggContainer)
		if state == containerStateRunning {
			fmt.Printf("  %s  yggdrasil  %s\n", styles.Success.Render("✓"), styles.StateTag(state))
		} else {
			fmt.Printf("  %s  yggdrasil  %s\n", styles.Err.Render("✗"), styles.StateTag(state))
			fmt.Printf("\n  Start with: %s\n\n", styles.Primary.Render("homelab start"))
			return nil
		}

		// Show active socat forwarders
		socatDir := filepath.Join(root, "yggdrasil", "socat.d")
		entries, err := os.ReadDir(socatDir)
		if err != nil {
			fmt.Printf("  %s  Could not read %s\n", styles.Warning.Render("!"), socatDir)
		} else {
			fmt.Printf("\n  %s\n", styles.Bold.Render("Active forwarders"))
			found := false
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".forward") {
					continue
				}
				name := strings.TrimSuffix(e.Name(), ".forward")
				data, _ := os.ReadFile(filepath.Join(socatDir, e.Name()))
				port := extractVar(string(data), "PORT")
				target := extractVar(string(data), "TARGET")
				fmt.Printf("  %s  %s → %s (TCP6:%s)\n",
					styles.Muted.Render("↳"),
					styles.Bold.Render(name),
					styles.Primary.Render(target),
					port,
				)
				found = true
			}
			if !found {
				fmt.Printf("  %s  (none — run %s)\n",
					styles.Muted.Render("!"),
					styles.Primary.Render("homelab ygg enable <service>"))
			}
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var yggLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Stream yggdrasil container logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		env := buildEnv(root, "")
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(root),
			env,
			withProfiles(root, "logs", "-f", yggContainer)...,
		)
	},
}

// ── enable ────────────────────────────────────────────────────────────────────

var yggEnablePort string

var yggEnableCmd = &cobra.Command{
	Use:   "enable <service>",
	Short: "Expose a service via Yggdrasil mesh node",
	Long: `Create a socat TCP6→TCP4 port forwarder on the Yggdrasil node.

Other Yggdrasil peers can reach the service at:
  [<yggdrasil-ipv6>]:<port>

Use --port to override the port detected from caddy.conf.`,
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

		// Write socat forwarder config
		socatDir := filepath.Join(root, "yggdrasil", "socat.d")
		if err := os.MkdirAll(socatDir, 0o750); err != nil {
			return fmt.Errorf("creating socat.d: %w", err)
		}
		fwdPath := filepath.Join(socatDir, name+".forward")
		content := fmt.Sprintf("PORT=%s\nTARGET=%s:%s\n", port, name, port)
		if err := os.WriteFile(fwdPath, []byte(content), 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", fwdPath, err)
		}
		fmt.Printf("  %s  %s written\n", styles.Success.Render("✓"), fwdPath)

		// Restart yggdrasil container to pick up new forwarders
		if err := RestartYgg(); err != nil {
			return fmt.Errorf("restarting yggdrasil: %w", err)
		}
		fmt.Printf("  %s  Yggdrasil restarted — forwarder active\n\n", styles.Success.Render("✓"))
		return nil
	},
}

// ── disable ───────────────────────────────────────────────────────────────────

var yggDisableCmd = &cobra.Command{
	Use:               "disable <service>",
	Short:             "Remove a Yggdrasil port forwarder",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		root := configDir()
		fwdPath := filepath.Join(root, "yggdrasil", "socat.d", name+".forward")

		if _, err := os.Stat(fwdPath); os.IsNotExist(err) {
			return fmt.Errorf("no forwarder found for %q", name)
		}
		if err := os.Remove(fwdPath); err != nil {
			return fmt.Errorf("removing %s: %w", fwdPath, err)
		}
		fmt.Printf("  %s  %s removed\n", styles.Warning.Render("→"), fwdPath)

		if err := RestartYgg(); err != nil {
			return fmt.Errorf("restarting yggdrasil: %w", err)
		}
		fmt.Printf("  %s  Yggdrasil restarted — forwarder removed\n\n", styles.Success.Render("✓"))
		return nil
	},
}

// ── list ──────────────────────────────────────────────────────────────────────

var yggListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active port forwarders",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		socatDir := filepath.Join(root, "yggdrasil", "socat.d")
		entries, err := os.ReadDir(socatDir)
		if err != nil {
			return fmt.Errorf("reading %s: %w", socatDir, err)
		}

		fmt.Printf("\n%s\n\n", styles.Header.Render("Yggdrasil Port Forwarders"))

		found := false
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".forward") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".forward")
			data, _ := os.ReadFile(filepath.Join(socatDir, e.Name()))
			port := extractVar(string(data), "PORT")
			target := extractVar(string(data), "TARGET")
			fmt.Printf("  %s  %s → %s (TCP6:%s)\n",
				styles.Dot(true, true),
				styles.Bold.Render(name),
				styles.Primary.Render(target),
				port,
			)
			found = true
		}
		if !found {
			fmt.Printf("  %s  (none)\n", styles.Muted.Render("!"))
		}
		fmt.Println()
		return nil
	},
}

// ── helpers ───────────────────────────────────────────────────────────────────

// AppendYggForwarder writes a socat forwarder config for a service and restarts yggdrasil.
func AppendYggForwarder(root, name, port string) error {
	socatDir := filepath.Join(root, "yggdrasil", "socat.d")
	if err := os.MkdirAll(socatDir, 0o750); err != nil {
		return fmt.Errorf("creating socat.d: %w", err)
	}
	fwdPath := filepath.Join(socatDir, name+".forward")
	content := fmt.Sprintf("PORT=%s\nTARGET=%s:%s\n", port, name, port)
	if err := os.WriteFile(fwdPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", fwdPath, err)
	}
	return nil
}

// RemoveYggForwarder removes a socat forwarder config and restarts yggdrasil.
func RemoveYggForwarder(root, name string) error {
	fwdPath := filepath.Join(root, "yggdrasil", "socat.d", name+".forward")
	if _, err := os.Stat(fwdPath); os.IsNotExist(err) {
		return nil
	}
	return os.Remove(fwdPath)
}

// RestartYgg restarts the yggdrasil container to reload forwarder configs.
func RestartYgg() error {
	root := configDir()
	env := buildEnv(root, "")
	return run.Default().DockerComposeEnv(
		run.CoreComposeFile(root),
		env,
		withProfiles(root, "restart", yggContainer)...,
	)
}

// extractVar extracts a shell variable value from a .forward file.
func extractVar(data, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func init() {
	yggEnableCmd.Flags().StringVar(&yggEnablePort, "port", "", "Override service port")
	yggCmd.AddCommand(yggStatusCmd)
	yggCmd.AddCommand(yggLogsCmd)
	yggCmd.AddCommand(yggListCmd)
	yggCmd.AddCommand(yggEnableCmd)
	yggCmd.AddCommand(yggDisableCmd)
}
