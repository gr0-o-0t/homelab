package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/ygg"
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
		if !extStatusHeader(root, "yggdrasil", yggContainer, "Yggdrasil Mesh Node") {
			return nil
		}

		addr := yggNodeAddress()
		if addr != "" {
			fmt.Printf("  %s  Address:  %s\n", styles.Muted.Render("↳"), styles.Bold.Render(addr))
		}

		fmt.Printf("\n  %s\n", styles.Bold.Render("Active forwarders"))
		if !printYggForwarders(root, addr) {
			fmt.Printf("  %s  (none — run %s)\n",
				styles.Muted.Render("!"),
				styles.Primary.Render("homelab ygg enable <service>"))
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var yggLogsCmd = containerLogsCmd(yggContainer,
	"Stream yggdrasil container logs")

// ── enable ────────────────────────────────────────────────────────────────────

var yggEnablePort string

var yggEnableCmd = &cobra.Command{
	Use:   "enable <service>",
	Short: "Expose a service via Yggdrasil mesh node",
	Long: `Create a socat TCP6→TCP4 port forwarder on the Yggdrasil node.

The forwarder relays to Caddy, which gets a matching :<port> site block, so
mesh peers reach the service at:
  http://[<yggdrasil-ipv6>]:<port>

The mesh port is allocated from 9000 up and stays put across re-enables — the
mesh has no naming, so a service is only addressable by port and two services
cannot share one. Use --port to override the service's own port (the upstream
Caddy proxies to), normally detected from caddy.conf.`,
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

		l, err := yggLayer()
		if err != nil {
			return err
		}
		// Same call path as `homelab enable <svc> --ygg`: this command used to
		// keep its own copy that wrote a forwarder straight to the service
		// container and no Caddy block at all.
		if err := l.Enable(name, name, network.ServiceInfo{},
			[]network.PortSelection{{Name: "default", Port: portNum, Protocol: "tcp"}}); err != nil {
			return err
		}
		if err := caddyReload(); err != nil {
			fmt.Printf("  %s  Caddy reload: %v\n", styles.Warning.Render("!"), err)
		}

		fmt.Printf("\n%s\n\n", styles.Header.Render("Yggdrasil: "+name))
		printYggForwarders(root, yggNodeAddress())
		fmt.Println()
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

		l, err := yggLayer()
		if err != nil {
			return err
		}
		if err := l.Disable(name); err != nil {
			return err
		}
		if err := caddyReload(); err != nil {
			fmt.Printf("  %s  Caddy reload: %v\n", styles.Warning.Render("!"), err)
		}
		fmt.Printf("  %s  %s removed from the mesh\n\n", styles.Warning.Render("→"), styles.Bold.Render(name))
		return nil
	},
}

// ── list ──────────────────────────────────────────────────────────────────────

var yggListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active port forwarders",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		fmt.Printf("\n%s\n\n", styles.Header.Render("Yggdrasil Port Forwarders"))
		if !printYggForwarders(root, yggNodeAddress()) {
			fmt.Printf("  %s  (none)\n", styles.Muted.Render("!"))
		}
		fmt.Println()
		return nil
	},
}

// printYggForwarders lists each forwarder as the URL a mesh peer can actually
// open, and reports whether it found any. addr is the node's mesh address, or
// "" when the node isn't running — the port is still worth showing.
func printYggForwarders(root, addr string) bool {
	socatDir := filepath.Join(root, "yggdrasil", "socat.d")
	entries, err := os.ReadDir(socatDir)
	if err != nil {
		return false
	}
	found := false
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".forward") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".forward")
		data, _ := os.ReadFile(filepath.Join(socatDir, e.Name())) //nolint:gosec // dir is ours
		port, _ := strconv.Atoi(extractVar(string(data), "PORT"))
		fmt.Printf("  %s  %s  %s\n",
			styles.Muted.Render("↳"),
			styles.Bold.Render(name),
			styles.Primary.Render(ygg.ServiceURL(addr, port)),
		)
		found = true
	}
	return found
}

// yggNodeAddress returns the node's mesh IPv6 address, or "" if the node isn't
// running or the admin endpoint doesn't answer. Cosmetic — never fatal.
func yggNodeAddress() string {
	l, err := yggLayer()
	if err != nil {
		return ""
	}
	return l.NodeAddress()
}

// yggLayer returns the registered Yggdrasil layer.
func yggLayer() (*ygg.Layer, error) {
	layer, ok := extRegistry().Get("ygg")
	if !ok {
		return nil, fmt.Errorf("ygg extension not registered")
	}
	l, ok := layer.(*ygg.Layer)
	if !ok {
		return nil, fmt.Errorf("unexpected ygg layer type %T", layer)
	}
	return l, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

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
