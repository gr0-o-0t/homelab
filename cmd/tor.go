package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/groot/homelab/internal/network/tor"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var torCmd = &cobra.Command{
	Use:   "tor",
	Short: "Manage Tor onion service proxy",
	Long:  "Inspect gnzsnz/torproxy and manage .onion hidden services.",
}

// ── status ────────────────────────────────────────────────────────────────────

var torStatusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"ps"},
	Short:   "Show Tor container and onion service status",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		if !extStatusHeader(root, "tor", "Tor Onion Service Proxy") {
			return nil
		}

		// List active onion services
		torrcDir := filepath.Join(root, "tor", "torrc.d")
		entries, err := os.ReadDir(torrcDir)
		if err != nil {
			fmt.Printf("  %s  Could not read %s\n", styles.Warning.Render("!"), torrcDir)
			return nil
		}

		fmt.Printf("\n  %s\n", styles.Bold.Render("Active onion services"))
		found := false
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".conf")
			onion := torOnionAddress(name)
			if onion == "" {
				continue
			}
			found = true
			fmt.Printf("  %s  %s → %s",
				styles.Muted.Render("↳"),
				styles.Bold.Render(name),
				styles.Primary.Render(onion),
			)
			fmt.Println()
		}
		if !found {
			fmt.Printf("  %s  (none — run %s)\n",
				styles.Muted.Render("!"),
				styles.Primary.Render("homelab enable <service> --tor"))
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var torLogsCmd = layerLogsCmd("tor", "Stream tor container logs")

// ── list ──────────────────────────────────────────────────────────────────────

var torListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active onion services with .onion addresses",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		torrcDir := filepath.Join(root, "tor", "torrc.d")
		entries, err := os.ReadDir(torrcDir)
		if err != nil {
			return fmt.Errorf("reading %s: %w", torrcDir, err)
		}

		fmt.Printf("\n%s\n\n", styles.Header.Render("Onion Services"))

		found := false
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".conf")
			onion := torOnionAddress(name)
			if onion == "" {
				fmt.Printf("  %s  %s  %s\n",
					styles.Warning.Render("!"), styles.Bold.Render(name),
					styles.Muted.Render("(container not running or service not yet ready)"))
				continue
			}
			found = true
			fmt.Printf("  %s  %s → %s\n",
				styles.Dot(true, true),
				styles.Bold.Render(name),
				styles.Primary.Render(onion),
			)
		}
		if !found {
			fmt.Printf("  %s  (none)\n", styles.Muted.Render("!"))
		}
		fmt.Println()
		return nil
	},
}

// ── helpers ───────────────────────────────────────────────────────────────────

// torLayer returns the registered Tor layer.
func torLayer() (*tor.Layer, error) {
	layer, ok := extRegistry().Get("tor")
	if !ok {
		return nil, fmt.Errorf("tor extension not registered")
	}
	l, ok := layer.(*tor.Layer)
	if !ok {
		return nil, fmt.Errorf("unexpected tor layer type %T", layer)
	}
	return l, nil
}

func init() {
	torCmd.AddCommand(torStatusCmd)
	torCmd.AddCommand(torLogsCmd)
	torCmd.AddCommand(torListCmd)
}
