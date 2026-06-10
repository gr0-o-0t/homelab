package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

const i2pContainer = "i2p"

var i2pCmd = &cobra.Command{
	Use:   i2pContainer,
	Short: "Manage i2pd router and eepsite tunnels",
	Long: `Inspect i2pd and manage eepsite tunnels via tunnels.conf.

Tunnels are defined in tunnels.conf as INI sections. After adding
or removing a tunnel, i2pd reloads config automatically (SIGHUP).

  homelab i2p enable  <service>   add HTTP eepsite tunnel
  homelab i2p disable <service>   remove eepsite tunnel
  homelab i2p list                show configured tunnels
  homelab i2p status              show router status
  homelab i2p logs                stream container logs`,
}

// ── paths ─────────────────────────────────────────────────────────────────────

// I2pTunnelsPath returns the path to the i2pd tunnels.conf.
func I2pTunnelsPath(root string) string {
	return filepath.Join(root, "i2p", "tunnels.conf")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ReloadI2pd sends SIGHUP to i2pd so it re-reads tunnels.conf.
func ReloadI2pd() error {
	return run.Default().DockerExec(i2pContainer, "kill", "-HUP", "1")
}

// TunnelSection is a parsed eepsite tunnel from tunnels.conf.
type TunnelSection struct {
	Name         string
	Host         string
	Port         string
	Keys         string
	HostOverride string
}

// ParseTunnels reads and parses tunnels.conf into sections.
func ParseTunnels(root string) ([]TunnelSection, error) {
	path := I2pTunnelsPath(root)
	data, err := os.ReadFile(path) // nosec G304 — path is constructed from configDir
	if err != nil {
		return nil, err
	}

	var tunnels []TunnelSection
	lines := strings.Split(string(data), "\n")

	reSection := regexp.MustCompile(`^\[(.+)\]$`)
	var current *TunnelSection
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "##") {
			if line == "" && current != nil && current.Host != "" {
				tunnels = append(tunnels, *current)
				current = nil
			}
			continue
		}

		if m := reSection.FindStringSubmatch(line); m != nil {
			if current != nil && current.Host != "" {
				tunnels = append(tunnels, *current)
			}
			current = &TunnelSection{Name: m[1]}
			continue
		}

		if current == nil {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "host":
			current.Host = val
		case "port":
			current.Port = val
		case "keys":
			current.Keys = val
		case "hostoverride":
			current.HostOverride = val
		}
	}

	if current != nil && current.Host != "" {
		tunnels = append(tunnels, *current)
	}

	return tunnels, nil
}

// SectionRange returns the start and end line indices (0-based, end-exclusive)
// of the section with the given name in the parsed lines.
func SectionRange(lines []string, name string) (int, int, bool) {
	re := regexp.MustCompile(`^\[` + regexp.QuoteMeta(name) + `\]$`)
	start := -1
	for i, line := range lines {
		if re.MatchString(strings.TrimSpace(line)) {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, false
	}

	end := start + 1
	for end < len(lines) {
		trimmed := strings.TrimSpace(lines[end])
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") && !strings.HasPrefix(trimmed, "##") {
			break
		}
		end++
	}

	return start, end, true
}

// AppendI2PTunnel appends an HTTP tunnel section to tunnels.conf.
// The tunnel routes .i2p traffic through Caddy:80 with hostoverride
// so Caddy can route by Host header.
func AppendI2PTunnel(root, name, port string) error {
	tunPath := I2pTunnelsPath(root)

	existing, err := ParseTunnels(root)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading tunnels.conf: %w", err)
	}
	for _, t := range existing {
		if t.Name == name {
			return fmt.Errorf("tunnel for %q already exists in tunnels.conf", name)
		}
	}

	f, err := os.OpenFile(tunPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening tunnels.conf: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Tunnel goes through Caddy:80 with hostoverride so Caddy routes by Host
	section := fmt.Sprintf("\n[%s]\ntype = http\nhost = caddy\nport = 80\nhostoverride = %s.i2p\nkeys = %s.dat\n",
		name, name, name)
	if _, err := f.WriteString(section); err != nil {
		return fmt.Errorf("writing tunnels.conf: %w", err)
	}
	return nil
}

// RemoveI2PTunnel removes a tunnel section from tunnels.conf.
func RemoveI2PTunnel(root, name string) error {
	tunPath := I2pTunnelsPath(root)

	data, err := os.ReadFile(tunPath) // nosec G304 — path is constructed from configDir
	if err != nil {
		return fmt.Errorf("reading tunnels.conf: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	start, end, found := SectionRange(lines, name)
	if !found {
		return fmt.Errorf("no tunnel for %q found in tunnels.conf", name)
	}

	removeStart := start
	if removeStart > 0 && strings.TrimSpace(lines[removeStart-1]) == "" {
		removeStart--
	}
	var newLines []string
	newLines = append(newLines, lines[:removeStart]...)
	newLines = append(newLines, lines[end:]...)
	if err := os.WriteFile(tunPath, []byte(strings.Join(newLines, "\n")), 0o600); err != nil {
		return fmt.Errorf("writing tunnels.conf: %w", err)
	}
	return nil
}

// ── status ────────────────────────────────────────────────────────────────────

var i2pStatusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"ps"},
	Short:   "Show i2pd router status",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		fmt.Printf("\n%s\n\n", styles.Header.Render("i2pd Router"))

		if !extEnabled(root, "i2p") {
			fmt.Printf("  %s  I2P not enabled.\n", styles.Warning.Render("!"))
			fmt.Printf("  Run %s to enable.\n\n",
				styles.Primary.Render("homelab ext enable i2p"))
			return nil
		}

		state := containerStatus(i2pContainer)
		if state == containerStateRunning {
			fmt.Printf("  %s  i2p  %s\n", styles.Success.Render("✓"), styles.StateTag(state))
			fmt.Printf("\n  %s  Web Console: %s\n",
				styles.Muted.Render("↳"),
				styles.Primary.Render("http://i2p:7070"))
		} else {
			fmt.Printf("  %s  i2p  %s\n", styles.Err.Render("✗"), styles.StateTag(state))
			fmt.Printf("\n  Start with: %s\n\n", styles.Primary.Render("homelab start"))
			return nil
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var i2pLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Stream i2pd container logs",
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
	Short: "Create an eepsite HTTP tunnel for a service",
	Long: `Add an HTTP eepsite tunnel to tunnels.conf and reload i2pd.

Traffic flows through Caddy:80 with hostoverride, so Caddy handles
routing by Host header. Use homelab enable <service> --i2p instead
for the full flow (Caddy route + i2pd tunnel + reload).`,
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

		if err := AppendI2PTunnel(root, name, port); err != nil {
			return err
		}

		fmt.Printf("\n%s\n\n", styles.Header.Render("I2P Eepsite: "+name))
		fmt.Printf("  %s  Tunnel:  %s → caddy:80 (hostoverride)\n",
			styles.Primary.Render("→"),
			styles.Bold.Render(name+".i2p"))
		fmt.Printf("  %s  Config:  %s\n",
			styles.Muted.Render("↳"),
			styles.Muted.Render(I2pTunnelsPath(root)))

		if containerStatus(i2pContainer) == containerStateRunning {
			if err := ReloadI2pd(); err != nil {
				fmt.Printf("  %s  Warning: reload failed (%v) — restart i2pd manually\n",
					styles.Warning.Render("!"), err)
			} else {
				fmt.Printf("  %s  i2pd reloaded\n", styles.Success.Render("✓"))
			}
		}
		fmt.Printf("\n  %s  Also create Caddy route: homelab enable %s --i2p\n\n",
			styles.Muted.Render("→"), name)
		return nil
	},
}

// ── disable ───────────────────────────────────────────────────────────────────

var i2pDisableCmd = &cobra.Command{
	Use:               "disable <service>",
	Short:             "Remove an eepsite tunnel from tunnels.conf",
	Long:              `Remove an HTTP eepsite tunnel from tunnels.conf and reload i2pd.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		root := configDir()

		err := RemoveI2PTunnel(root, name)
		if err != nil {
			return err
		}

		fmt.Printf("\n  %s  Tunnel %q removed from tunnels.conf\n",
			styles.Warning.Render("→"), styles.Bold.Render(name+".i2p"))

		if containerStatus(i2pContainer) == containerStateRunning {
			if err := ReloadI2pd(); err != nil {
				fmt.Printf("  %s  Warning: reload failed (%v)\n",
					styles.Warning.Render("!"), err)
			} else {
				fmt.Printf("  %s  i2pd reloaded\n", styles.Success.Render("✓"))
			}
		}
		fmt.Println()
		return nil
	},
}

// ── list ──────────────────────────────────────────────────────────────────────

var i2pListCmd = &cobra.Command{
	Use:   "list",
	Short: "List eepsite tunnels from tunnels.conf",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		fmt.Printf("\n%s\n\n", styles.Header.Render("i2pd Eepsite Tunnels"))

		if !extEnabled(root, "i2p") {
			fmt.Printf("  %s  I2P not enabled.\n", styles.Warning.Render("!"))
			fmt.Printf("  Run %s to enable.\n\n",
				styles.Primary.Render("homelab ext enable i2p"))
			return nil
		}

		tunnels, err := ParseTunnels(root)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("  %s  No tunnels.conf — run setup first\n", styles.Muted.Render("!"))
				return nil
			}
			return fmt.Errorf("reading tunnels.conf: %w", err)
		}

		if len(tunnels) == 0 {
			fmt.Printf("  %s  No eepsite tunnels configured.\n", styles.Muted.Render("!"))
			fmt.Printf("  %s  Run %s to create one.\n\n",
				styles.Muted.Render("→"),
				styles.Primary.Render("homelab i2p enable <service>"))
			return nil
		}

		for _, t := range tunnels {
			target := fmt.Sprintf("%s:%s", t.Host, t.Port)
			if t.HostOverride != "" {
				target = fmt.Sprintf("%s:%s (hostoverride %s)", t.Host, t.Port, t.HostOverride)
			}
			fmt.Printf("  %s  %s → %s  [keys: %s]\n",
				styles.Dot(true, true),
				styles.Bold.Render(t.Name+".i2p"),
				target, t.Keys,
			)
		}

		fmt.Printf("\n  %s  Config: %s\n",
			styles.Muted.Render("↳"),
			styles.Muted.Render(I2pTunnelsPath(root)))
		fmt.Println()
		return nil
	},
}

func init() {
	i2pCmd.AddCommand(i2pStatusCmd)
	i2pCmd.AddCommand(i2pLogsCmd)
	i2pCmd.AddCommand(i2pEnableCmd)
	i2pCmd.AddCommand(i2pDisableCmd)
	i2pCmd.AddCommand(i2pListCmd)

	i2pEnableCmd.Flags().StringVar(&i2pEnablePort, "port", "", "Override service port")
}
