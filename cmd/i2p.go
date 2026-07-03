package cmd

import (
	"fmt"
	"os"
	"strconv"

	"github.com/groot/homelab/internal/network/i2p"
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

// i2pLayer fetches the registered i2p network layer, type-asserting to the
// concrete type so callers can reach the tunnels.conf helpers that aren't
// part of the generic network.NetworkLayer interface. Delegating here (
// instead of duplicating ParseTunnels/AppendTunnel/RemoveTunnel/etc. in this
// file) is deliberate: this package used to keep its own copy of that logic,
// which diverged from internal/network/i2p.Layer's (missing a MkdirAll,
// disagreeing on remove-of-missing-tunnel semantics) and broke the exact
// workflow this command's own help text suggests — see AppendTunnel's doc.
func i2pLayer() (*i2p.Layer, error) {
	layer, ok := extRegistry().Get("i2p")
	if !ok {
		return nil, fmt.Errorf("i2p extension not registered")
	}
	l, ok := layer.(*i2p.Layer)
	if !ok {
		return nil, fmt.Errorf("unexpected i2p layer type %T", layer)
	}
	return l, nil
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
		portNum, err := strconv.Atoi(port)
		if err != nil {
			return fmt.Errorf("invalid port %q: %w", port, err)
		}

		l, err := i2pLayer()
		if err != nil {
			return err
		}
		if err := l.AppendTunnel(name, portNum); err != nil {
			return err
		}

		fmt.Printf("\n%s\n\n", styles.Header.Render("I2P Eepsite: "+name))
		fmt.Printf("  %s  Tunnel:  %s → caddy:80 (hostoverride)\n",
			styles.Primary.Render("→"),
			styles.Bold.Render(name+".i2p"))
		fmt.Printf("  %s  Config:  %s\n",
			styles.Muted.Render("↳"),
			styles.Muted.Render(l.TunnelsPath()))

		if containerStatus(i2pContainer) == containerStateRunning {
			if err := l.Reload(); err != nil {
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

		l, err := i2pLayer()
		if err != nil {
			return err
		}
		if err := l.RemoveTunnel(name); err != nil {
			return err
		}

		fmt.Printf("\n  %s  Tunnel %q removed from tunnels.conf\n",
			styles.Warning.Render("→"), styles.Bold.Render(name+".i2p"))

		if containerStatus(i2pContainer) == containerStateRunning {
			if err := l.Reload(); err != nil {
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

		l, err := i2pLayer()
		if err != nil {
			return err
		}
		tunnels, err := l.ParseTunnels()
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
			styles.Muted.Render(l.TunnelsPath()))
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
