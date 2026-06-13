package cmd

import (
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

// extContainer maps extension names to their Docker Compose service/container name
// as defined in the core docker-compose.yml.
var extContainer = map[string]string{
	"cf":        "cloudflared",
	"tor":       torContainer,
	"i2p":       i2pContainer,
	"ygg":       yggContainer,
	"yggdrasil": yggContainer,
	"ipfs":      ipfsContainer,
}

// extProfile maps extension names to their Docker Compose profile name.
var extProfile = map[string]string{
	"cf":   "tunnel",
	"tor":  "tor",
	"i2p":  "i2p",
	"ygg":  "yggdrasil",
	"ipfs": "ipfs",
}

var extCmd = &cobra.Command{
	Use:   "ext",
	Short: "Manage network extensions",
	Long: `Manage network extensions that add exposure layers to the core stack.

Extensions:
  cf   Cloudflare Tunnel  (public internet via cloudflared)
  tor  Tor onion service  (.onion addresses)
  i2p  I2P eepsite proxy  (.i2p addresses)
  ygg  Yggdrasil mesh     (IPv6 mesh)
  ipfs IPFS Kubo node     (content-addressed storage)

Commands:
  ext list                 List extensions and their enabled/disabled status
  ext status [ext]         Show container status for all or one extension
  ext logs [ext]           Stream logs for all or one extension
  ext start [ext]          Start extension containers
  ext stop [ext]           Stop extension containers

Extension-specific subcommands:
  ext cf route             Manage Cloudflare DNS routes
  ext ipfs gateway         Manage IPFS Gateway Caddy route

Service-level exposure is managed via the root enable/disable command:
  homelab enable <svc> --i2p    expose via I2P eepsite
  homelab enable <svc> --tor    expose as Tor .onion service
  homelab enable <svc> --ygg    expose on Yggdrasil mesh
  homelab disable <svc> --i2p   remove I2P eepsite`,
}

// ── valid extensions ─────────────────────────────────────────────────────────

func validExtNames() []string {
	return []string{"cf", "tor", "i2p", "ygg", "yggdrasil", "ipfs"}
}

func completeExtNames(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return validExtNames(), cobra.ShellCompDirectiveNoFileComp
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

// ── status ────────────────────────────────────────────────────────────────────

var extStatusCmd = &cobra.Command{
	Use:   "status [extension]",
	Short: "Show extension container status",
	Long: `Show whether each extension's container is running.

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
		for _, ext := range targets {
			state := containerStatus(extContainer[ext])
			label := config.ExtensionLabel(ext)
			if state == containerStateRunning {
				fmt.Printf("  %s  %s  %s\n",
					styles.Success.Render("✓"),
					styles.Bold.Render(ext),
					label)
			} else {
				fmt.Printf("  %s  %s  %s  [%s]\n",
					styles.Muted.Render("·"),
					ext,
					label,
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
	Short: "Stream extension container logs",
	Long: `Stream logs from extension containers.

Without an argument, shows logs for all enabled extensions.
With an extension name, shows logs for that specific extension.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		env := buildEnv(dir, "")

		targets, err := resolveExtTargets(args)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			return fmt.Errorf("no extensions enabled or specified")
		}

		logArgs := []string{"logs", "-f"}
		for _, ext := range targets {
			container := extContainer[ext]
			if container != "" {
				logArgs = append(logArgs, container)
			}
		}
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, logArgs...)...,
		)
	},
}

// ── start ─────────────────────────────────────────────────────────────────────

var extBuild bool

var extStartCmd = &cobra.Command{
	Use:   "start [extension]",
	Short: "Start extension containers",
	Long: `Start extension Docker containers.

  homelab ext start          # start all enabled extensions
  homelab ext start tor      # start Tor container`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runExtStartStop(args, false, extBuild)
	},
}

// ── stop ─────────────────────────────────────────────────────────────────────

var extStopCmd = &cobra.Command{
	Use:   "stop [extension]",
	Short: "Stop extension containers",
	Long: `Stop extension Docker containers.

  homelab ext stop           # stop all enabled extensions
  homelab ext stop tor       # stop Tor container`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeExtNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runExtStartStop(args, true, false)
	},
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolveExtTargets returns extension names to operate on. With no args,
// returns all currently enabled extensions from config.
func resolveExtTargets(args []string) ([]string, error) {
	if len(args) > 0 {
		ext := args[0]
		if _, ok := extContainer[ext]; !ok {
			return nil, fmt.Errorf("unknown extension %q\n\nAvailable: %s", ext, strings.Join(validExtNames(), ", "))
		}
		return []string{ext}, nil
	}
	cfg, err := config.Load(rootConfigFile())
	if err != nil || cfg == nil {
		return nil, nil
	}
	return cfg.Extensions, nil
}

// runExtStartStop starts or stops extension containers via docker compose.
// build forces image rebuild before starting (no-op for stop).
func runExtStartStop(args []string, stop bool, build bool) error {
	dir := configDir()
	env := buildEnv(dir, "")

	// Determine which extension(s) to act on.
	extNames, err := resolveExtTargets(args)
	if err != nil {
		return err
	}
	if len(extNames) == 0 {
		return fmt.Errorf("no extensions enabled or specified")
	}

	// Collect unique profiles.
	seen := make(map[string]bool)
	var profiles []string
	for _, ext := range extNames {
		p := extProfile[ext]
		if p != "" && !seen[p] {
			seen[p] = true
			profiles = append(profiles, "--profile", p)
		}
	}

	if stop {
		fmt.Printf("%s Stopping extension containers…\n", styles.Warning.Render("→"))
		containers := make([]string, len(extNames))
		for i, ext := range extNames {
			containers[i] = extContainer[ext]
		}
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			append([]string{"stop"}, containers...)...,
		)
	}

	fmt.Printf("%s Starting extension containers…\n", styles.Primary.Render("→"))
	upArgs := []string{"up", "-d"}
	if build {
		upArgs = append(upArgs, "--build")
	}
	return run.Default().DockerComposeEnv(
		run.CoreComposeFile(dir),
		env,
		append(profiles, upArgs...)...,
	)
}

// ── init ──────────────────────────────────────────────────────────────────────

func init() {
	extStartCmd.Flags().BoolVar(&extBuild, "build", false, "Rebuild images before starting")

	extCmd.AddCommand(
		extListCmd,
		extStatusCmd,
		extLogsCmd,
		extStartCmd,
		extStopCmd,
	)

	// Register full per-layer commands at root level (replaces ext hub).
	// extCmd is kept for source backward compat but not registered on root.
	rootCmd.AddCommand(cfCmd)
	rootCmd.AddCommand(torCmd)
	rootCmd.AddCommand(i2pCmd)
	rootCmd.AddCommand(yggCmd)
	rootCmd.AddCommand(ipfsCmd)
}
