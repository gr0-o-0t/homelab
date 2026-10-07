package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/groot/homelab/internal/network/ygg"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var yggCmd = &cobra.Command{
	Use:     "ygg",
	Aliases: []string{"yggdrasil"},
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
		if !extStatusHeader(root, "ygg", "Yggdrasil Mesh Node") {
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
				styles.Primary.Render("homelab enable <service> --ygg"))
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var yggLogsCmd = layerLogsCmd("ygg", "Stream yggdrasil container logs")

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
	yggCmd.AddCommand(yggStatusCmd)
	yggCmd.AddCommand(yggLogsCmd)
	yggCmd.AddCommand(yggListCmd)
}
