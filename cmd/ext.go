package cmd

import (
	"fmt"
	"sort"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

// validExts is the set of extensions that can be enabled/disabled.
// Must match config.AllExtensions().
var validExts = map[string]string{
	"cf":        "Cloudflare Tunnel",
	"tor":       "Tor onion service proxy",
	"i2p":       "I2P router + eepsite proxy",
	"yggdrasil": "Yggdrasil mesh node",
	"ipfs":      "IPFS Kubo node",
}

var extCmd = &cobra.Command{
	Use:   "ext",
	Short: "Manage optional network extensions",
	Long: `Enable, disable, and list optional core stack extensions.

Extensions add network exposure layers to the core stack:
  cf         Cloudflare Tunnel (public internet via cloudflared)
  tor        Tor onion service proxy (.onion addresses)
  i2p        I2P router + eepsite proxy (.i2p addresses)
  yggdrasil  Yggdrasil IPv6 mesh node
  ipfs       IPFS Kubo node (content-addressed storage)

Enable an extension:
  homelab ext enable tor
  homelab ext enable cf

Disable an extension:
  homelab ext disable i2p

List enabled extensions:
  homelab ext list`,
}

// ── list ──────────────────────────────────────────────────────────────────────

var extListCmd = &cobra.Command{
	Use:   "list",
	Short: "List enabled network extensions",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(rootConfigFile())
		if err != nil {
			return err
		}

		fmt.Printf("\n%s\n\n", styles.Header.Render("Network Extensions"))

		all := config.AllExtensions()
		enabled := make(map[string]bool)
		if cfg != nil {
			for _, e := range cfg.Extensions {
				enabled[e] = true
			}
		}

		for _, name := range all {
			label := config.ExtensionLabel(name)
			if enabled[name] {
				fmt.Printf("  %s  %-12s  %s\n",
					styles.Success.Render("✓"),
					styles.Bold.Render(name),
					label)
			} else {
				fmt.Printf("  %s  %-12s  %s\n",
					styles.Muted.Render("·"),
					name,
					label)
			}
		}
		fmt.Println()
		return nil
	},
}

// ── enable ────────────────────────────────────────────────────────────────────

var extEnableCmd = &cobra.Command{
	Use:   "enable <extension>",
	Short: "Enable a network extension in the core stack",
	Long: `Mark a network extension as enabled.

After enabling, run 'homelab core start' to activate the extension
(or 'homelab core restart' if the core stack is already running).

Available extensions: cf, tor, i2p, yggdrasil, ipfs`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if _, ok := validExts[name]; !ok {
			return fmt.Errorf("unknown extension %q\n\nAvailable: cf, tor, i2p, yggdrasil, ipfs", name)
		}

		cfgFile := rootConfigFile()
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("reading config: %w", err)
		}
		if cfg == nil {
			cfg = &config.Config{}
		}

		if cfg.HasExtension(name) {
			fmt.Printf("  %s  %s already enabled\n",
				styles.Muted.Render("!"), styles.Bold.Render(name))
			fmt.Println()
			return nil
		}

		cfg.EnableExtension(name)
		if err := config.Save(cfgFile, cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}

		fmt.Printf("  %s  %s enabled\n",
			styles.Success.Render("✓"), styles.Bold.Render(config.ExtensionLabel(name)))
		fmt.Printf("  %s  Run %s to activate\n\n",
			styles.Muted.Render("→"), styles.Primary.Render("homelab core restart"))
		return nil
	},
}

// ── disable ───────────────────────────────────────────────────────────────────

var extDisableCmd = &cobra.Command{
	Use:   "disable <extension>",
	Short: "Disable a network extension",
	Long: `Remove a network extension from the core stack.

After disabling, run 'homelab core down && homelab core start' to fully
stop and remove the extension's containers.

Available extensions: cf, tor, i2p, yggdrasil, ipfs`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if _, ok := validExts[name]; !ok {
			return fmt.Errorf("unknown extension %q\n\nAvailable: cf, tor, i2p, yggdrasil, ipfs", name)
		}

		cfgFile := rootConfigFile()
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("reading config: %w", err)
		}
		if cfg == nil {
			fmt.Printf("  %s  %s not enabled\n",
				styles.Muted.Render("!"), styles.Bold.Render(name))
			fmt.Println()
			return nil
		}

		if !cfg.HasExtension(name) {
			fmt.Printf("  %s  %s not currently enabled\n",
				styles.Muted.Render("!"), styles.Bold.Render(name))
			fmt.Println()
			return nil
		}

		cfg.DisableExtension(name)
		if err := config.Save(cfgFile, cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}

		fmt.Printf("  %s  %s disabled\n",
			styles.Warning.Render("→"), styles.Bold.Render(config.ExtensionLabel(name)))
		fmt.Printf("  %s  Run %s to stop containers\n\n",
			styles.Muted.Render("→"), styles.Primary.Render("homelab core down && homelab core start"))
		return nil
	},
}

// ── completion ────────────────────────────────────────────────────────────────

func completeExtNames(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	names := make([]string, 0, len(validExts))
	for n := range validExts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}

func init() {
	extCmd.AddCommand(extListCmd, extEnableCmd, extDisableCmd)
	rootCmd.AddCommand(extCmd)
}
