package cmd

import (
	"fmt"
	"os"
	"strconv"

	"github.com/groot/homelab/internal/network/i2p"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

const i2pContainer = "i2p"

var i2pCmd = &cobra.Command{
	Use:   i2pContainer,
	Short: "Manage i2pd router and eepsite tunnels",
	Long: `Inspect i2pd and manage eepsite tunnels via tunnels.conf.

Tunnels are defined in tunnels.conf as INI sections. After adding
or removing a tunnel, i2pd is restarted to pick up the change.

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
		if !extStatusHeader(configDir(), "i2p", i2pContainer, "i2pd Router") {
			return nil
		}
		fmt.Printf("\n  %s  Web Console: %s\n\n",
			styles.Muted.Render("↳"),
			styles.Primary.Render("http://127.0.0.1:7070"))
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var i2pLogsCmd = containerLogsCmd(i2pContainer,
	"Stream i2pd container logs")

// ── enable ────────────────────────────────────────────────────────────────────

var i2pEnablePort string

var i2pEnableCmd = &cobra.Command{
	Use:   "enable <service>",
	Short: "Create an eepsite HTTP tunnel for a service",
	Long: `Add an HTTP eepsite tunnel to tunnels.conf and restart i2pd.

Traffic flows to Caddy (via tailscale:80, Caddy's shared network
namespace) with hostoverride, so Caddy routes by Host header. Use
homelab enable <service> --i2p instead for the full flow
(Caddy route + i2pd tunnel + reload).`,
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
		fmt.Printf("  %s  Config:  %s\n",
			styles.Muted.Render("↳"),
			styles.Muted.Render(l.TunnelsPath()))

		if containerStatus(i2pContainer) == containerStateRunning {
			if err := l.Reload(); err != nil {
				fmt.Printf("  %s  Warning: reload failed (%v) — restart i2pd manually\n",
					styles.Warning.Render("!"), err)
			} else {
				fmt.Printf("  %s  i2pd restarted\n", styles.Success.Render("✓"))
			}
		}

		// The b32 is the address. Printing the .i2p host as though it were one
		// is what sent a browser to a stranger's eepsite: nothing publishes
		// that name, and a router whose addressbook has it goes wherever its
		// registrant pointed it.
		printI2PAddress(l, name)
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
	i2pCmd.AddCommand(i2pEnableCmd)
	i2pCmd.AddCommand(i2pDisableCmd)
	i2pCmd.AddCommand(i2pListCmd)

	i2pEnableCmd.Flags().StringVar(&i2pEnablePort, "port", "", "Override service port")
}

// printI2PAddress prints the eepsite's real address, plus the Host header it
// answers on for context.
func printI2PAddress(l *i2p.Layer, name string) {
	for _, a := range l.ServiceAddresses(name, nil) {
		if a.Note == "" {
			fmt.Printf("  %s  Address: %s\n",
				styles.Primary.Render("→"), styles.Bold.Render(a.URL))
			continue
		}
		fmt.Printf("  %s  %s  %s\n",
			styles.Muted.Render("↳"), a.URL, styles.Muted.Render("("+a.Note+")"))
	}
}
