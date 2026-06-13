package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/secrets"
	"github.com/spf13/cobra"
)

var rootFlags struct {
	configDir  string
	configFile string
	noColor    bool
	json       bool
}

var (
	rootCfg *config.Config
	cfgMu   sync.Mutex
)

var rootCmd = &cobra.Command{
	Use:   "homelab",
	Short: "Self-hosted services manager",
	Long: `homelab manages your self-hosted service stack backed by Tailscale and Caddy.

Run without arguments to open the interactive service browser.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		if isTTY() && !rootFlags.json {
			return runListTUI(dir)
		}
		svcs, err := discoverServices(dir)
		if err != nil {
			return err
		}
		if rootFlags.json {
			return printServiceJSON(svcs)
		}
		env := buildEnv(dir, "")
		printServiceTable(svcs, env, false)
		return nil
	},
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		os.Exit(1)
	}
}

// RootCmd returns the root cobra command for testing.
func RootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	rootCmd.PersistentFlags().StringVar(&rootFlags.configDir, "config-dir", "",
		"homelab config directory (default: ${XDG_CONFIG_HOME:-$HOME/.config}/homelab)")
	rootCmd.PersistentFlags().StringVar(&rootFlags.configFile, "config", "",
		"root config file; overrides config-dir/config.yaml")
	rootCmd.PersistentFlags().BoolVar(&rootFlags.noColor, "no-color", false,
		"disable coloured output")
	rootCmd.PersistentFlags().BoolVar(&rootFlags.json, "json", false,
		"output as JSON (on commands that support it)")

	rootCmd.AddCommand(serviceAddCmd)
	rootCmd.AddCommand(serviceNewCmd)
	rootCmd.AddCommand(serviceUpdateCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(pullCmd)
	rootCmd.AddCommand(execCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(imagesCmd)
	rootCmd.AddCommand(portCmd)
	rootCmd.AddCommand(restartCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(reloadCmd)
	rootCmd.AddCommand(validateCmd)

	initExtensions()
	// Register root-level commands for layers without full cmd trees.
	// cf/tor/i2p/ygg/ipfs register their own full commands via ext.go init().
	if cmd := extCommandFor("ts"); cmd != nil {
		rootCmd.AddCommand(cmd)
	}
}

// configDir returns the effective homelab config directory.
// Priority: --config-dir > dir of --config > XDG default.
func configDir() string {
	if rootFlags.configDir != "" {
		return rootFlags.configDir
	}
	if rootFlags.configFile != "" {
		return filepath.Dir(rootFlags.configFile)
	}
	return config.DefaultConfigDir()
}

// rootConfigFile returns the effective root config.yaml path.
// --config takes priority over configDir/config.yaml.
func rootConfigFile() string {
	return config.RootConfigFile(configDir(), rootFlags.configFile)
}

// noColor reports whether coloured output should be suppressed.
func noColor() bool {
	return rootFlags.noColor || os.Getenv("NO_COLOR") != ""
}

// extEnabled checks whether a named extension is enabled in the root config.
// Cache loaded config to avoid re-parsing config.yaml on every call.
// Retries on failure (mutex, not sync.Once).
func extEnabled(cfgDir, name string) bool {
	cfgMu.Lock()
	if rootCfg == nil {
		cfg, err := config.Load(config.RootConfigFile(cfgDir, rootFlags.configFile))
		if err == nil {
			rootCfg = cfg
		}
	}
	cfg := rootCfg
	cfgMu.Unlock()
	if cfg == nil {
		return false
	}
	return cfg.HasExtension(name)
}

// buildEnv assembles the full docker compose environment map.
// Non-fatal errors (keyring unavailable, missing config) are logged to stderr
// so a partially-configured setup still starts containers.
func buildEnv(cfgDir, svcName string) map[string]string {
	sm, err := secrets.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: keyring unavailable (%v)\n", err)
	}
	env, err := config.BuildEnv(rootConfigFile(), cfgDir, svcName, sm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: config error (%v)\n", err)
	}
	if env == nil {
		env = make(map[string]string)
	}
	// Ensure critical vars always have a value, even when config loading
	// fails or the YAML is missing entries. Docker Compose variable
	// substitution (:-default) is a secondary fallback, but not all
	// versions handle empty-string values identically â€” being explicit
	// here prevents the fragile case where {$HOME_SUBDOMAIN} resolves to
	// "" and Caddy produces an invalid double-dot domain.
	if env["HOME_SUBDOMAIN"] == "" {
		env["HOME_SUBDOMAIN"] = "home"
	}
	return env
}
