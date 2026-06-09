package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/groot/homelab/assets"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/db"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/tui/spinner"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// ── homelab setup ─────────────────────────────────────────────────────────────

var setupCmd = &cobra.Command{
	Use:   "setup [service]",
	Short: "Configure homelab or service variables and secrets",
	Long: `Configure homelab root settings, or a specific service's config.

Without a service argument, runs the homelab root setup wizard.
With a service argument, runs the per-service setup wizard.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return runServiceSetup(cmd, args)
		}
		return runSetup(cmd, args)
	},
}

func runSetup(_ *cobra.Command, _ []string) error {
	dir := configDir()
	cfgFile := rootConfigFile()

	fmt.Printf("\n%s\n\n", styles.Header.Render("Homelab Setup"))
	fmt.Printf("  %s\n", styles.Muted.Render("Non-secret values → "+cfgFile))
	fmt.Printf("  %s\n\n", styles.Muted.Render("Secrets → system keyring"))

	// Load existing config for defaults.
	cfg, _ := config.Load(cfgFile)
	if cfg == nil {
		cfg = &config.Config{
			Vars: map[string]config.VarEntry{
				"DOMAIN":         {Required: true},
				"HOME_SUBDOMAIN": {Value: "home", Required: true},
				"ACME_EMAIL":     {Required: true},
				"TS_HOSTNAME":    {Value: "caddy-home", Required: true},
				"PUB_SUBDOMAIN":  {Value: "pub", Required: false},
				"CF_TUNNEL_NAME": {Value: "", Required: false},
				"I2P_EXT_PORT":   {Value: "45678", Required: false},
			},
			Secrets: map[string]config.SecretEntry{
				"TS_AUTHKEY":           {Required: true},
				"CLOUDFLARE_API_TOKEN": {Required: true},
				"CF_TUNNEL_TOKEN":      {Required: false},
			},
		}
	}

	sm, err := secrets.Open()
	if err != nil {
		return fmt.Errorf("opening keyring: %w\n\nEnsure a keyring backend is available (see docs/setup.md)", err)
	}

	sc := bufio.NewScanner(os.Stdin)

	// ── Configuration ─────────────────────────────────────────────────────────
	fmt.Printf("  %s\n\n", styles.Accent.Render("─── Configuration ──────────────────────────────────"))

	varOrder := []string{"DOMAIN", "HOME_SUBDOMAIN", "ACME_EMAIL", "TS_HOSTNAME"}
	labels := map[string]string{
		"DOMAIN":         "Domain (e.g. example.com)",
		"HOME_SUBDOMAIN": "Subdomain prefix",
		"ACME_EMAIL":     "ACME / Let's Encrypt email",
		"TS_HOSTNAME":    "Tailscale hostname",
	}
	for _, k := range varOrder {
		e := cfg.Vars[k]
		e.Value = promptStr(sc, labels[k], e.Value)
		cfg.Vars[k] = e
	}

	// ── Core secrets ──────────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n\n", styles.Accent.Render("─── Secrets ────────────────────────────────────────"))
	for _, k := range []string{"TS_AUTHKEY", "CLOUDFLARE_API_TOKEN"} {
		isSet := sm.IsSet("", k)
		if val := promptSecret(k, isSet); val != "" {
			if err := sm.Set("", k, val); err != nil {
				return fmt.Errorf("storing %s in keyring: %w", k, err)
			}
		}
	}

	// ── Network Extensions (optional) ─────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Accent.Render("─── Network Extensions (optional) ────────────────────"))
	fmt.Printf("  %s\n\n", styles.Muted.Render("Enable alternative network exposure. Press Enter to skip."))

	extNames := []struct {
		Name  string
		Label string
	}{
		{"cf", "Cloudflare Tunnel (public internet via cloudflared)"},
		{torContainer, "Tor onion service proxy (.onion addresses)"},
		{i2pContainer, "I2P router + eepsite proxy (.i2p addresses)"},
		{yggContainer, "Yggdrasil IPv6 mesh node (socat port forwarding)"},
		{ipfsContainer, "IPFS Kubo node (content-addressed P2P storage)"},
	}
	for _, ext := range extNames {
		added := cfg.HasExtension(ext.Name)
		var prompt string
		if added {
			prompt = "n/Y"
		} else {
			prompt = "y/N"
		}
		fmt.Printf("  Add %s? [%s]: ", ext.Label, prompt)
		var answer string
		if sc.Scan() {
			answer = strings.TrimSpace(sc.Text())
		}
		if added {
			// Already added: n removes, anything else (Enter) keeps
			if strings.EqualFold(answer, "n") {
				cfg.DisableExtension(ext.Name)
			} else {
				cfg.EnableExtension(ext.Name)
			}
		} else {
			// Not added: y adds, anything else (Enter) skips
			if strings.EqualFold(answer, "y") {
				cfg.EnableExtension(ext.Name)
			} else {
				cfg.DisableExtension(ext.Name)
			}
		}
	}

	// ── Cloudflare configuration (only when cf extension enabled) ────────────
	if cfg.HasExtension("cf") {
		fmt.Printf("\n  %s\n\n", styles.Accent.Render("─── Cloudflare Tunnel configuration ─────────────────"))

		pubEntry := cfg.Vars["PUB_SUBDOMAIN"]
		pubEntry.Value = promptStr(sc, "Public subdomain prefix", pubEntry.Value)
		cfg.Vars["PUB_SUBDOMAIN"] = pubEntry

		tunnelNameEntry := cfg.Vars["CF_TUNNEL_NAME"]
		tunnelNameEntry.Value = promptStr(sc, "Cloudflare Tunnel name (from dash.cloudflare.com, or press Enter to skip)", tunnelNameEntry.Value)
		cfg.Vars["CF_TUNNEL_NAME"] = tunnelNameEntry

		if val := promptSecret("CF_TUNNEL_TOKEN", sm.IsSet("", "CF_TUNNEL_TOKEN")); val != "" {
			if err := sm.Set("", "CF_TUNNEL_TOKEN", val); err != nil {
				return fmt.Errorf("storing CF_TUNNEL_TOKEN in keyring: %w", err)
			}
		}
	}

	// ── Persist config ────────────────────────────────────────────────────────
	fmt.Println()
	if err := config.Save(cfgFile, cfg); err != nil {
		return err
	}
	step(styles.Success.Render("✓"), "config.yaml written to "+dir)
	step(styles.Success.Render("✓"), "Secrets stored in keyring")

	// ── Install core assets ───────────────────────────────────────────────────
	fmt.Printf("\n  %s\n\n", styles.Accent.Render("─── Installing core files ──────────────────────────"))
	if err := installAssets(dir); err != nil {
		step(styles.Err.Render("✗"), fmt.Sprintf("Installing assets: %v", err))
	} else {
		step(styles.Success.Render("✓"), "core/ installed to "+filepath.Join(dir, "core"))
		step(styles.Success.Render("✓"), "caddy/ installed to "+filepath.Join(dir, "caddy"))
		step(styles.Success.Render("✓"), "tor/ installed to "+filepath.Join(dir, "tor"))
		step(styles.Success.Render("✓"), "i2p/ installed to "+filepath.Join(dir, "i2p"))
		step(styles.Success.Render("✓"), "yggdrasil/ installed to "+filepath.Join(dir, "yggdrasil"))
	}

	// ── Infrastructure ────────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n\n", styles.Accent.Render("─── Infrastructure ─────────────────────────────────"))

	exists, err := run.DockerNetworkExists("home-services")
	if err != nil {
		step(styles.Warning.Render("!"), fmt.Sprintf("Could not check Docker network: %v", err))
	} else if exists {
		step(styles.Success.Render("✓"), "Docker network 'home-services' already exists")
	} else {
		if spinErr := spinner.Run("Creating Docker network 'home-services'…", func() error {
			return run.Default().DockerNetworkCreate("home-services")
		}); spinErr != nil {
			step(styles.Err.Render("✗"), fmt.Sprintf("Creating network failed: %v", spinErr))
		} else {
			step(styles.Success.Render("✓"), "Docker network 'home-services' created")
		}
	}

	if _, err := os.Stat("/dev/net/tun"); os.IsNotExist(err) {
		step(styles.Warning.Render("!"), "/dev/net/tun not found — run: sudo modprobe tun")
	} else {
		step(styles.Success.Render("✓"), "/dev/net/tun present")
	}

	// ── Next steps ────────────────────────────────────────────────────────────
	fmt.Printf("\n%s\n", styles.Header.Render("Setup complete — next steps:"))
	fmt.Printf("  1. %s\n", styles.Primary.Render("homelab add <name>   # install a bundled service"))
	fmt.Printf("  2. %s\n", styles.Primary.Render("homelab setup <name>"))
	fmt.Printf("  3. %s\n", styles.Primary.Render("homelab start"))
	fmt.Printf("  4. %s   %s\n",
		styles.Primary.Render("homelab status"),
		styles.Muted.Render("— verify Tailscale joined your tailnet"))
	fmt.Printf("  5. %s\n", styles.Primary.Render("homelab up <name>"))
	fmt.Printf("  6. %s\n\n", styles.Primary.Render("homelab enable <name>"))
	return nil
}

// ── homelab service setup ─────────────────────────────────────────────────────

var serviceSetupCmd = &cobra.Command{
	Use:               "setup <service>",
	Short:             "Configure variables and secrets for a service",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	Long: `Interactive wizard for a single service's configuration.

Non-secret values are saved to services/<name>/config.yaml in the config dir.
Secrets are stored in the system keyring.

Variables declared in the root config.yaml are inherited automatically
and only need to be set here if you want to override them for this service.`,
	RunE: runServiceSetup,
}

func runServiceSetup(_ *cobra.Command, args []string) error {
	name := args[0]
	dir := configDir()

	if err := validateService(dir, name); err != nil {
		return err
	}

	fmt.Printf("\n%s %s\n\n",
		styles.Header.Render("Service Setup:"),
		styles.Bold.Render(name))

	svcCfgFile := config.ServiceConfigFile(dir, name)
	svcCfg, err := config.Load(svcCfgFile)
	if err != nil {
		return err
	}
	if svcCfg == nil {
		svcCfg = &config.Config{
			Vars:    make(map[string]config.VarEntry),
			Secrets: make(map[string]config.SecretEntry),
		}
	}
	if len(svcCfg.Vars) == 0 && len(svcCfg.Secrets) == 0 {
		fmt.Printf("  %s\n\n",
			styles.Muted.Render("No variables declared in config.yaml — nothing to configure."))
		return nil
	}

	sm, err := secrets.Open()
	if err != nil {
		return fmt.Errorf("opening keyring: %w", err)
	}

	sc := bufio.NewScanner(os.Stdin)

	// ── Non-secret vars ───────────────────────────────────────────────────────
	if len(svcCfg.Vars) > 0 {
		fmt.Printf("  %s\n\n", styles.Accent.Render("─── Configuration ──────────────────────────────────"))
		for k, e := range svcCfg.Vars {
			lbl := k
			if !e.Required {
				lbl += " (optional)"
			}
			e.Value = promptStr(sc, lbl, e.Value)
			svcCfg.Vars[k] = e
		}
	}

	// ── Secrets ───────────────────────────────────────────────────────────────
	if len(svcCfg.Secrets) > 0 {
		fmt.Printf("\n  %s\n\n", styles.Accent.Render("─── Secrets ────────────────────────────────────────"))
		for k, e := range svcCfg.Secrets {
			lbl := k
			if !e.Required {
				lbl += " (optional)"
			}
			if val := promptSecret(lbl, sm.IsSet(name, k)); val != "" {
				if err := sm.Set(name, k, val); err != nil {
					return fmt.Errorf("storing %s in keyring: %w", k, err)
				}
			}
		}
	}

	// ── Persist ───────────────────────────────────────────────────────────────
	fmt.Println()
	if err := config.Save(svcCfgFile, svcCfg); err != nil {
		return err
	}
	step(styles.Success.Render("✓"), fmt.Sprintf("services/%s/config.yaml written", name))
	if len(svcCfg.Secrets) > 0 {
		step(styles.Success.Render("✓"), "Secrets stored in keyring")
	}

	// ── Database provisioning ─────────────────────────────────────────────────
	ctx := context.Background()
	p := db.New(dir, sm)

	if svcCfg.Databases.Kind != 0 {
		svcDB, err := svcCfg.ServiceDatabases()
		if err != nil {
			return fmt.Errorf("reading database declarations: %w", err)
		}
		if len(svcDB) > 0 {
			fmt.Printf("\n  %s\n\n", styles.Accent.Render("─── Database Setup ──────────────────────────────────"))
			for dbType, decl := range svcDB {
				if err := p.EnsureRunning(ctx, dbType); err != nil {
					step(styles.Warning.Render("!"), fmt.Sprintf("%s container not running — install and start first:", dbType))
					fmt.Printf("    homelab add %s && homelab up %s\n", dbType, dbType)
					continue
				}
				if err := p.Provision(ctx, dbType, name, decl); err != nil {
					step(styles.Err.Render("✗"), fmt.Sprintf("Failed to provision %s: %v", dbType, err))
				} else {
					step(styles.Success.Render("✓"), fmt.Sprintf("%s database '%s' created with user '%s'",
						dbType, decl.Database, decl.User))
				}
			}
		}
	}

	fmt.Println()
	return nil
}

func init() {
	serviceCmd.AddCommand(serviceSetupCmd)
}

func step(icon, msg string) {
	fmt.Printf("  %s  %s\n", icon, msg)
}

// installAssets copies the embedded core and caddy trees from assets.CoreFS
// into configDir. Existing files are overwritten so that `homelab setup` can
// be re-run to update them.
func installAssets(configDir string) error {
	return fs.WalkDir(assets.CoreFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(configDir, path)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o750)
		}
		data, err := assets.CoreFS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o600)
	})
}

// ── prompt helpers ────────────────────────────────────────────────────────────

func promptStr(sc *bufio.Scanner, label, current string) string {
	if current != "" {
		fmt.Printf("  %s [%s]: ", label, styles.Muted.Render(current))
	} else {
		fmt.Printf("  %s: ", label)
	}
	if sc.Scan() {
		if v := strings.TrimSpace(sc.Text()); v != "" {
			return v
		}
	}
	return current
}

func promptSecret(label string, isSet bool) string {
	if isSet {
		fmt.Printf("  %s [%s]: ",
			label, styles.Muted.Render("already set, press Enter to keep"))
	} else {
		fmt.Printf("  %s: ", label)
	}
	val, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() {
			return strings.TrimSpace(sc.Text())
		}
		return ""
	}
	return strings.TrimSpace(string(val))
}
