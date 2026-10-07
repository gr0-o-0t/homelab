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
With a service argument, runs the per-service setup wizard.

Non-interactive (front ends, scripts):
  homelab setup [service] --json                 # declared vars + secrets; secret values never shown
  homelab setup [service] --set KEY=VALUE ...    # write vars without prompting
  echo '{"TOKEN":"…"}' | homelab setup [service] --secrets-stdin
Only declared names are accepted; an empty secret value leaves it unchanged.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return runServiceSetup(cmd, args)
		}
		return runSetup(cmd, args)
	},
}

func runSetup(cmd *cobra.Command, _ []string) error {
	dir := configDir()
	cfgFile := rootConfigFile()

	if err := checkSetupFlags(); err != nil {
		return err
	}

	// Load existing config for defaults. A parse error must stop setup: it
	// would otherwise fall through to the defaults below and overwrite the
	// file, losing groups, extensions and databases over one typo.
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("%w — fix the file, setup will not overwrite it", err)
	}
	if cfg == nil {
		cfg = rootSetupDefaults()
	}

	sm, err := openSecrets()
	if err != nil {
		return fmt.Errorf("opening keyring: %w\n\nEnsure a keyring backend is available (see docs/setup.md)", err)
	}

	if rootFlags.json {
		return printSetupJSON(cmd.OutOrStdout(), "", withRootDeclarations(cfg), sm)
	}

	fmt.Printf("\n%s\n\n", styles.Header.Render("Homelab Setup"))
	fmt.Printf("  %s\n", styles.Muted.Render("Non-secret values → "+cfgFile))
	fmt.Printf("  %s\n\n", styles.Muted.Render("Secrets → system keyring"))

	if setupNonInteractive() {
		if err := applySetup(cmd, withRootDeclarations(cfg), "", sm); err != nil {
			return err
		}
	} else if err := promptRootSetup(cfg, sm); err != nil {
		return err
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

// rootSetupDefaults is the root config a first `homelab setup` starts from.
func rootSetupDefaults() *config.Config {
	return &config.Config{
		Vars: map[string]config.VarEntry{
			"DOMAIN":         {Required: true},
			"HOME_SUBDOMAIN": {Value: "home", Required: true},
			"ACME_EMAIL":     {Required: true},
			"TS_HOSTNAME":    {Value: "caddy-home", Required: true},
			"CF_TUNNEL_NAME": {Value: "pub", Required: false},
			"I2P_EXT_PORT":   {Value: "45678", Required: false},
			// i2pd's web console, published on loopback only. Overridable
			// because a host-level i2pd (the distro package runs one as a
			// systemd service) already owns 7070, and the container then
			// cannot bind it. `homelab doctor` names the collision.
			"I2P_CONSOLE_PORT": {Value: "7070", Required: false},
		},
		Secrets: map[string]config.SecretEntry{
			"TS_AUTHKEY":           {Required: true},
			"CLOUDFLARE_API_TOKEN": {Required: true},
			"CF_TUNNEL_TOKEN":      {Required: false},
		},
	}
}

// rootSetupLabels describe the root settings; also the descriptions
// `setup --json` reports for them.
//
//nolint:gosec // G101: human-readable labels for secret names, not credentials
var rootSetupLabels = map[string]string{
	"DOMAIN":               "Domain (e.g. example.com)",
	"HOME_SUBDOMAIN":       "Subdomain prefix",
	"ACME_EMAIL":           "ACME / Let's Encrypt email",
	"TS_HOSTNAME":          "Tailscale hostname",
	"CF_TUNNEL_NAME":       "Cloudflare Tunnel name (from dash.cloudflare.com)",
	"I2P_EXT_PORT":         "I2P router external port",
	"I2P_CONSOLE_PORT":     "i2pd web console port (loopback)",
	"TS_AUTHKEY":           "Tailscale auth key",
	"CLOUDFLARE_API_TOKEN": "Cloudflare API token (DNS:Edit, for certificates)",
	"CF_TUNNEL_TOKEN":      "Cloudflare Tunnel token",
}

// promptRootSetup is the interactive half of root setup: it asks for each
// setting and stores secrets as they are entered.
func promptRootSetup(cfg *config.Config, sm *secrets.Manager) error {
	sc := bufio.NewScanner(os.Stdin)

	// ── Configuration ─────────────────────────────────────────────────────────
	fmt.Printf("  %s\n\n", styles.Accent.Render("─── Configuration ──────────────────────────────────"))

	for _, k := range []string{"DOMAIN", "HOME_SUBDOMAIN", "ACME_EMAIL", "TS_HOSTNAME"} {
		e := cfg.Vars[k]
		e.Value = promptStr(sc, rootSetupLabels[k], e.Value)
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
		{"tor", "Tor onion service proxy (.onion addresses)"},
		{"i2p", "I2P router + eepsite proxy (.i2p addresses)"},
		{"yggdrasil", "Yggdrasil IPv6 mesh node (socat port forwarding)"},
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

		tunnelNameEntry := cfg.Vars["CF_TUNNEL_NAME"]
		tunnelNameEntry.Value = promptStr(sc, rootSetupLabels["CF_TUNNEL_NAME"], tunnelNameEntry.Value)
		cfg.Vars["CF_TUNNEL_NAME"] = tunnelNameEntry

		if val := promptSecret("CF_TUNNEL_TOKEN", sm.IsSet("", "CF_TUNNEL_TOKEN")); val != "" {
			if err := sm.Set("", "CF_TUNNEL_TOKEN", val); err != nil {
				return fmt.Errorf("storing CF_TUNNEL_TOKEN in keyring: %w", err)
			}
		}
	}
	return nil
}

// ── homelab service setup ─────────────────────────────────────────────────────

func runServiceSetup(cmd *cobra.Command, args []string) error {
	name := args[0]
	dir := configDir()

	if err := validateService(dir, name); err != nil {
		return err
	}
	if cmd != nil { // nil from `homelab add`, which never passes setup flags
		if err := checkSetupFlags(); err != nil {
			return err
		}
	}

	svcCfgFile := config.ServiceConfigFile(dir, name)
	svcCfg, err := config.Load(svcCfgFile)
	if err != nil {
		return err
	}
	if svcCfg == nil {
		svcCfg = &config.Config{}
	}
	if svcCfg.Vars == nil {
		svcCfg.Vars = make(map[string]config.VarEntry)
	}
	if svcCfg.Secrets == nil {
		svcCfg.Secrets = make(map[string]config.SecretEntry)
	}

	sm, err := openSecrets()
	if err != nil {
		return fmt.Errorf("opening keyring: %w", err)
	}

	if cmd != nil && rootFlags.json {
		return printSetupJSON(cmd.OutOrStdout(), name, svcCfg, sm)
	}

	fmt.Printf("\n%s %s\n\n",
		styles.Header.Render("Service Setup:"),
		styles.Bold.Render(name))

	if len(svcCfg.Vars) == 0 && len(svcCfg.Secrets) == 0 {
		if cmd != nil && setupNonInteractive() {
			// Nothing is declared, so anything given is unknown.
			return applySetup(cmd, svcCfg, name, sm)
		}
		fmt.Printf("  %s\n\n",
			styles.Muted.Render("No variables declared in config.yaml — nothing to configure."))
		return nil
	}

	if cmd != nil && setupNonInteractive() {
		if err := applySetup(cmd, svcCfg, name, sm); err != nil {
			return err
		}
	} else if err := promptServiceSetup(name, svcCfg, sm); err != nil {
		return err
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
	if err := ensureGeneratedSecrets(p, name); err != nil {
		return err
	}

	if svcCfg.Databases.Kind != 0 {
		svcDB, err := svcCfg.ServiceDatabases()
		if err != nil {
			return fmt.Errorf("reading database declarations: %w", err)
		}
		if len(svcDB) > 0 {
			fmt.Printf("\n  %s\n\n", styles.Accent.Render("─── Database Setup ──────────────────────────────────"))
			for i := range svcDB {
				entry := &svcDB[i]
				shared := config.SharedDBName(entry.Type)
				if err := p.EnsureRunning(ctx, entry.Type); err != nil {
					step(styles.Warning.Render("!"), fmt.Sprintf("%s container not running — install and start first:", shared))
					fmt.Printf("    homelab add %s && homelab up %s\n", shared, shared)
					continue
				}
				if err := p.Provision(ctx, entry.Type, name, entry.ServiceDBDecl); err != nil {
					step(styles.Err.Render("✗"), fmt.Sprintf("Failed to provision %s: %v", entry.Type, err))
					continue
				}
				switch entry.Type {
				case config.DBRedis:
					step(styles.Success.Render("✓"), "redis database number allocated")
				case config.DBS3:
					bucket := entry.Bucket
					if bucket == "" {
						bucket = name
					}
					step(styles.Success.Render("✓"), fmt.Sprintf("garage bucket '%s' and access key ready", bucket))
				default:
					step(styles.Success.Render("✓"), fmt.Sprintf("%s database '%s' created with user '%s'",
						entry.Type, entry.Database, entry.User))
				}
			}
		}
	}

	fmt.Println()
	return nil
}

// promptServiceSetup is the interactive half of service setup.
func promptServiceSetup(name string, svcCfg *config.Config, sm *secrets.Manager) error {
	sc := bufio.NewScanner(os.Stdin)

	// ── Non-secret vars ───────────────────────────────────────────────────────
	if len(svcCfg.Vars) > 0 {
		fmt.Printf("  %s\n\n", styles.Accent.Render("─── Configuration ──────────────────────────────────"))
		for _, k := range sortedKeys(svcCfg.Vars) {
			e := svcCfg.Vars[k]
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
		for _, k := range sortedKeys(svcCfg.Secrets) {
			e := svcCfg.Secrets[k]
			if e.Generate != "" {
				// Minted by homelab (below / on `up`); asking would only invite
				// a weaker hand-typed value.
				step(styles.Muted.Render("·"), fmt.Sprintf("%s is generated automatically", k))
				continue
			}
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
	return nil
}

func init() {
	setupCmd.Flags().StringArrayVar(&setupFlags.set, "set", nil,
		"Set a variable without prompting, KEY=VALUE (repeatable)")
	setupCmd.Flags().BoolVar(&setupFlags.secretsStdin, "secrets-stdin", false,
		`Read secrets as a JSON object {"NAME":"value"} from stdin, without prompting`)
}

func step(icon, msg string) {
	fmt.Printf("  %s  %s\n", icon, msg)
}

// installAssets copies the embedded core trees from assets.CoreFS into
// configDir. Files homelab owns outright are overwritten so re-running setup,
// or `homelab update`, picks up a new version; the rest are only created when
// missing — see assetIsManaged.
func installAssets(configDir string) error {
	// Pre-create services/ so Docker (running as root) doesn't create it
	// via the Caddy volume mount, which would make it root-owned and break
	// `homelab add <service>`.
	if err := os.MkdirAll(filepath.Join(configDir, "services"), 0o750); err != nil {
		return fmt.Errorf("creating services dir: %w", err)
	}
	return fs.WalkDir(assets.CoreFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(configDir, path)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o750)
		}
		if _, err := os.Stat(dest); err == nil && !assetIsManaged(path) {
			return nil
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

// assetIsManaged reports whether an embedded core file is homelab's to
// overwrite: the compose file, Dockerfiles and entrypoints, the Caddyfile, and
// READMEs. Everything else under tor/, i2p/ and yggdrasil/ is a starting point
// that becomes the user's — i2p/tunnels.conf holds every service's eepsite
// tunnel, and torrc, i2pd.conf and yggdrasil.conf are meant to be edited — so
// overwriting them on update would silently drop exposure or local changes.
func assetIsManaged(path string) bool {
	return strings.HasPrefix(path, "core/") || path == "caddy/Caddyfile" || filepath.Base(path) == "README"
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
