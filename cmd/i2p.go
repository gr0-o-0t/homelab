package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/network/i2p"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var i2pCmd = &cobra.Command{
	Use:   "i2p",
	Short: "Manage i2pd router and eepsite tunnels",
	Long: `Inspect i2pd and manage eepsite tunnels via tunnels.conf.

Tunnels are defined in tunnels.conf as INI sections. After adding
or removing a tunnel, i2pd is sent SIGHUP to reload tunnels.conf in place.

  homelab enable <service> --i2p   add an eepsite (tunnel + Caddy route)
  homelab disable <service> --i2p  remove it
  homelab i2p list                show configured tunnels
  homelab i2p status              show router status
  homelab i2p logs                stream container logs`,
}

// i2pLayer fetches the registered i2p network layer, type-asserting to the
// concrete type so callers can reach the tunnels.conf helpers that aren't
// part of the generic network.NetworkLayer interface.
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
		if !extStatusHeader(configDir(), "i2p", "i2pd Router") {
			return nil
		}
		fmt.Printf("\n  %s  Web Console: %s\n\n",
			styles.Muted.Render("↳"),
			styles.Primary.Render("http://127.0.0.1:7070"))
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var i2pLogsCmd = layerLogsCmd("i2p", "Stream i2pd container logs")

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
				styles.Primary.Render("homelab enable <service> --i2p"))
			return nil
		}

		for _, t := range tunnels {
			// The b32 is the address: it is derived from the tunnel's key and
			// any I2P client can open it. The .i2p name is only the Host
			// header i2pd stamps on incoming requests so Caddy can vhost —
			// nothing publishes it, and if it resolves for anyone it resolves
			// to whoever registered that name, not to you.
			addr := l.B32Address(t.Name)
			if addr == "" {
				fmt.Printf("  %s  %s  %s\n",
					styles.Warning.Render("!"), styles.Bold.Render(t.Name),
					styles.Muted.Render("(destination not built yet — is i2pd running?)"))
				continue
			}
			fmt.Printf("  %s  %s  %s\n",
				styles.Dot(true, true),
				styles.Bold.Render(t.Name),
				styles.Primary.Render("http://"+addr),
			)
			fmt.Printf("      %s → %s:%s   host header %s\n",
				styles.Muted.Render("via Caddy"),
				t.Host, t.Port,
				styles.Muted.Render(t.HostOverride),
			)
			// Opening this once through the router's HTTP proxy registers the
			// name in that router's addressbook, after which the plain host
			// works in the browser. It is the only way a name resolves.
			if jump := l.AddressHelperURL(t.Name); jump != "" {
				fmt.Printf("      %s %s\n",
					styles.Muted.Render("register the name (open once via the i2p proxy):"),
					styles.Muted.Render(jump))
			}
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
	i2pCmd.AddCommand(i2pListCmd)
}
