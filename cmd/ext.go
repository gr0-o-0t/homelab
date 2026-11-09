package cmd

import (
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var extCmd = &cobra.Command{
	Use:   "ext",
	Short: "Manage network extensions",
	Long: `Manage network extensions that add exposure layers beside the tailnet.

Extensions:
  cf   Cloudflare Tunnel  (public internet via cloudflared, a Swarm service)

Tor, I2P and Yggdrasil are not available in cluster mode yet.

Commands:
  ext enable <ext>         Turn an extension on and deploy it
  ext disable <ext>        Remove it and turn it off
  ext list                 List extensions and their enabled/disabled status
  ext status [ext]         Show the state of all or one extension
  ext logs [ext]           Stream logs for all or one extension

Extension-specific management lives under its own top-level command:
  homelab cf route add <svc>     Add a Cloudflare DNS route
  homelab cf status              Cloudflare Tunnel status

Service-level exposure is managed via the root enable/disable command:
  homelab enable <svc> --cf     expose via Cloudflare Tunnel
  homelab disable <svc> --cf    remove it`,
}

// ── valid extensions ─────────────────────────────────────────────────────────

func completeExtNames(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return extNames(), cobra.ShellCompDirectiveNoFileComp
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

		for _, l := range extRegistry().All() {
			name, label := l.Name(), l.Label()
			if hasResolvedExtension(cfg, name) {
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

// ── status ────────────────────────────────────────────────────────────────────

var extStatusCmd = &cobra.Command{
	Use:   "status [extension]",
	Short: "Show extension status",
	Long: `Show whether each extension runs.

Without an argument, shows status for every enabled extension.
With an extension name, show status for that specific extension.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		targets, err := resolveExtTargets(args)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			fmt.Println(styles.Muted.Render("\n  No extensions enabled.\n"))
			return nil
		}
		fmt.Println()
		for _, l := range targets {
			state := l.Status().ContainerState
			if state == containerStateRunning {
				fmt.Printf("  %s  %s  %s\n",
					styles.Success.Render("✓"),
					styles.Bold.Render(l.Name()),
					l.Label())
			} else {
				fmt.Printf("  %s  %s  %s  [%s]\n",
					styles.Muted.Render("·"),
					l.Name(),
					l.Label(),
					styles.Muted.Render(state))
			}
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var extLogsCmd = &cobra.Command{
	Use:   "logs [extension]",
	Short: "Stream extension logs",
	Long: `Stream logs from extensions.

Without an argument, shows logs for all enabled extensions.
With an extension name, shows logs for that specific extension.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		targets, err := resolveExtTargets(args)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			return fmt.Errorf("no extensions enabled or specified")
		}
		// Every extension available in cluster mode is a Swarm service
		// (cloudflared): its logs come from `docker service logs`.
		for _, l := range targets {
			if err := serviceLogsCLI(cmdContext(), []string{"service", cmdLogs, "--follow", l.ContainerName()},
				cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
		}
		return nil
	},
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolveExtTargets returns the layers to operate on. With no args, returns
// every extension enabled in config.yaml.
func resolveExtTargets(args []string) ([]network.NetworkLayer, error) {
	if len(args) > 0 {
		l, err := optionalLayer(args[0])
		if err != nil {
			return nil, err
		}
		return []network.NetworkLayer{l}, nil
	}
	cfg, err := config.Load(rootConfigFile())
	if err != nil || cfg == nil {
		return nil, nil
	}
	var out []network.NetworkLayer
	for _, ext := range cfg.Extensions {
		if l, err := optionalLayer(ext); err == nil {
			out = append(out, l)
		}
	}
	return out, nil
}

// optionalLayer resolves an `ext` argument to a layer an ext command may
// turn on and off — never the private tailnet, which is the agent's own.
func optionalLayer(name string) (network.NetworkLayer, error) {
	l, err := layerByName(name)
	if err == nil && l.Flag() == "" {
		err = fmt.Errorf("unknown extension %q\n\nAvailable: %s", name, strings.Join(extNames(), ", "))
	}
	return l, err
}

// ── enable / disable ─────────────────────────────────────────────────────────

var extEnableCmd = &cobra.Command{
	Use:               "enable <ext>",
	Short:             "Turn an extension on and deploy it",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE:              func(_ *cobra.Command, args []string) error { return setExtEnabled(args[0], true) },
}

var extDisableCmd = &cobra.Command{
	Use:               "disable <ext>",
	Short:             "Remove an extension and turn it off",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE:              func(_ *cobra.Command, args []string) error { return setExtEnabled(args[0], false) },
}

// setExtEnabled records the extension in config.yaml — the same list `homelab
// setup` edits — and deploys or removes it to match. Services still need
// `homelab enable <svc> --<ext>` to be exposed on it.
func setExtEnabled(ext string, on bool) error {
	if _, err := optionalLayer(ext); err != nil {
		return err
	}
	ext = config.ResolveExtension(ext)
	cfgFile := rootConfigFile()
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config.yaml — run homelab setup first")
	}
	root := configDir()
	if !on {
		if ext == cloudflaredLayer {
			fmt.Printf("%s Stopping cloudflared…\n", styles.Primary.Render("→"))
			if err := stopCloudflared(root); err != nil {
				return err
			}
		}
		cfg.DisableExtension(ext)
		return config.Save(cfgFile, cfg)
	}
	cfg.EnableExtension(ext)
	if err := config.Save(cfgFile, cfg); err != nil {
		return err
	}
	if ext == cloudflaredLayer {
		fmt.Printf("%s Starting cloudflared…\n", styles.Primary.Render("→"))
		if err := startCloudflared(root, buildEnv(root, "")); err != nil {
			return err
		}
	}
	fmt.Printf("%s %s enabled — expose a service with %s\n", styles.Success.Render("✓"),
		extLabel(ext), styles.Primary.Render("homelab enable <svc> --"+ext))
	return nil
}

// ── init ──────────────────────────────────────────────────────────────────────

func init() {
	extCmd.AddCommand(
		extEnableCmd,
		extDisableCmd,
		extListCmd,
		extStatusCmd,
		extLogsCmd,
	)

	rootCmd.AddCommand(extCmd)
	rootCmd.AddCommand(cfCmd)
}
