package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/spinner"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

// ── homelab doctor ────────────────────────────────────────────────────────────

var doctorFixFlag bool
var doctorAllFlag bool

var doctorCmd = &cobra.Command{
	Use:   "doctor [service]",
	Short: "Check homelab or service health",
	Long: `Runs health checks. Without arguments, checks the homelab environment.
With a service name, checks that specific service.

  homelab doctor          # core stack + environment
  homelab doctor jellyfin # single service
  homelab doctor --all    # all installed services

Pass --fix to automatically repair safe issues (missing network, broken symlinks).`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runDoctor,
}

func runDoctor(_ *cobra.Command, args []string) error {
	dir := configDir()

	// Service doctor — --all or named service
	if doctorAllFlag || len(args) > 0 {
		if doctorAllFlag {
			svcs, err := service.Discover(dir)
			if err != nil {
				return err
			}
			if len(svcs) == 0 {
				fmt.Println(styles.Muted.Render("\n  No services found.\n"))
				return nil
			}
			var failed []string
			for _, svc := range svcs {
				ok := runServiceDoctorFor(dir, svc.Name, doctorFixFlag)
				if !ok {
					failed = append(failed, svc.Name)
				}
			}
			fmt.Println()
			if len(failed) > 0 {
				fmt.Printf("  %s %s\n\n",
					styles.Err.Render("Checks failed for:"),
					strings.Join(failed, ", "))
			} else {
				fmt.Printf("  %s\n\n", styles.Success.Render("All services healthy."))
			}
			return nil
		}
		// Single service
		runServiceDoctorFor(dir, args[0], doctorFixFlag)
		return nil
	}

	cfgFile := rootConfigFile()

	fmt.Printf("\n%s\n\n", styles.Header.Render("Homelab Health Check"))

	sm, err := secrets.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: keyring unavailable (%v)\n", err)
	}

	pass := true
	check := func(ok bool, msg string) {
		if ok {
			fmt.Printf("  %s  %s\n", styles.Success.Render("✓"), msg)
		} else {
			fmt.Printf("  %s  %s\n", styles.Err.Render("✗"), msg)
			pass = false
		}
	}
	warn := func(msg string) {
		fmt.Printf("  %s  %s\n", styles.Warning.Render("!"), msg)
	}

	// ── Configuration ─────────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Bold.Render("Configuration"))

	cfg, err := config.Load(cfgFile)
	check(err == nil, "config.yaml readable")
	if cfg != nil {
		for k, e := range cfg.Vars {
			if e.Required {
				check(e.Value != "", k+" is set")
			}
		}
		for k, e := range cfg.Secrets {
			isSet := sm != nil && sm.IsSet("", k)
			if e.Required {
				check(isSet, k+" is set in keyring")
			} else if !isSet {
				warn(k + " is not set (optional)")
			}
		}
	} else if err == nil {
		check(false, "config.yaml not found — run 'homelab setup'")
	}

	// ── Infrastructure ────────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Bold.Render("Infrastructure"))

	check(dockerDaemonUp(), "Docker daemon is running")

	netExists, _ := run.DockerNetworkExists("home-services")
	check(netExists, "Network 'home-services' exists")
	if !netExists && doctorFixFlag {
		if err := spinner.Run("Creating Docker network 'home-services'…", func() error {
			return run.Default().DockerNetworkCreate("home-services")
		}); err != nil {
			warn(fmt.Sprintf("Could not create network: %v", err))
		} else {
			fmt.Printf("  %s  Network 'home-services' created\n", styles.Success.Render("✓"))
			pass = true
		}
	}

	_, tunErr := os.Stat("/dev/net/tun")
	check(tunErr == nil, "/dev/net/tun present")

	// ── Caddy routing dirs ────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Bold.Render("Caddy routing"))

	caddyConfD := filepath.Join(dir, "caddy", "conf.d")
	caddyConfDPub := filepath.Join(dir, "caddy", "conf.d-cf")

	for _, d := range []string{caddyConfD, caddyConfDPub} {
		rel, _ := filepath.Rel(dir, d)
		if _, err := os.Stat(d); os.IsNotExist(err) {
			check(false, rel+" directory present")
			if doctorFixFlag {
				if mkErr := os.MkdirAll(d, 0o750); mkErr == nil {
					fmt.Printf("  %s  %s created\n", styles.Success.Render("✓"), rel)
				} else {
					warn(fmt.Sprintf("Could not create %s: %v", rel, mkErr))
				}
			}
		} else {
			check(true, rel+" directory present")
			brokenCount := removeBrokenSymlinks(d, doctorFixFlag)
			if brokenCount > 0 {
				fmt.Printf("  %s  %s: removed %d broken symlink(s)\n",
					styles.Success.Render("✓"), rel, brokenCount)
			}
		}
	}

	// ── Core Stack ────────────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Bold.Render("Core Stack"))

	check(fileExistsAt(run.CoreComposeFile(dir)), "core/docker-compose.yml present")

	tsState := containerStatus("tailscale")
	check(tsState == containerStateRunning, fmt.Sprintf("tailscale container %s", stateLabel(tsState)))

	caddyState := containerStatus("caddy")
	caddyRunning := caddyState == containerStateRunning
	check(caddyRunning, fmt.Sprintf("caddy container %s", stateLabel(caddyState)))

	if caddyRunning {
		var buf strings.Builder
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		valErr := caddy.NewWithRunner(dir, r).Validate()
		check(valErr == nil, "Caddy config valid")
	}

	if tsState == containerStateRunning {
		ip, connected := tailscaleIP()
		if connected {
			check(true, fmt.Sprintf("Tailscale connected (%s)", ip))
		} else {
			check(false, "Tailscale not connected")
		}
	}

	// ── Network Extensions ────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Bold.Render("Network Extensions"))

	for _, layer := range extRegistry.All() {
		name := layer.Name()
		if cfg != nil && hasResolvedExtension(cfg, name) {
			cName := layer.ContainerName()
			cState := containerStatus(cName)
			check(cState == containerStateRunning, fmt.Sprintf("%s (%s) container %s",
				config.ExtensionLabel(name), cName, stateLabel(cState)))
		}
	}

	// ── Result ────────────────────────────────────────────────────────────────
	fmt.Println()
	if pass {
		fmt.Printf("  %s\n\n", styles.Success.Render("All checks passed."))
	} else {
		if !doctorFixFlag {
			fmt.Printf("  %s\n", styles.Err.Render("Some checks failed — see above."))
			fmt.Printf("  %s\n\n", styles.Muted.Render("Run with --fix to auto-repair safe issues."))
		} else {
			fmt.Printf("  %s\n\n", styles.Err.Render("Some checks failed — manual intervention may be required."))
		}
	}
	return nil
}

// ── homelab service doctor ────────────────────────────────────────────────────

var serviceDoctorFlags struct {
	all bool
	fix bool
}

var serviceDoctorCmd = &cobra.Command{
	Use:               "doctor [service]",
	Short:             "Check health of a specific service (or all with --all)",
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()

		if serviceDoctorFlags.all {
			svcs, err := service.Discover(dir)
			if err != nil {
				return err
			}
			if len(svcs) == 0 {
				fmt.Println(styles.Muted.Render("\n  No services found.\n"))
				return nil
			}
			var failed []string
			for _, svc := range svcs {
				ok := runServiceDoctorFor(dir, svc.Name, serviceDoctorFlags.fix)
				if !ok {
					failed = append(failed, svc.Name)
				}
			}
			fmt.Println()
			if len(failed) > 0 {
				fmt.Printf("  %s %s\n\n",
					styles.Err.Render("Checks failed for:"),
					strings.Join(failed, ", "))
			} else {
				fmt.Printf("  %s\n\n", styles.Success.Render("All services healthy."))
			}
			return nil
		}

		if len(args) == 0 {
			return fmt.Errorf("service name required (or use --all)")
		}
		runServiceDoctorFor(dir, args[0], serviceDoctorFlags.fix)
		return nil
	},
}

func runServiceDoctorFor(dir, name string, fix bool) bool {
	fmt.Printf("\n%s %s\n\n",
		styles.Header.Render("Service Health:"),
		styles.Bold.Render(name))

	sm, err := secrets.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: keyring unavailable (%v)\n", err)
	}

	pass := true
	check := func(ok bool, msg string) {
		if ok {
			fmt.Printf("  %s  %s\n", styles.Success.Render("✓"), msg)
		} else {
			fmt.Printf("  %s  %s\n", styles.Err.Render("✗"), msg)
			pass = false
		}
	}

	// ── Configuration ─────────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Bold.Render("Configuration"))

	svcDir := config.ServiceConfigFile(dir, name)
	check(fileExistsAt(strings.TrimSuffix(svcDir, "/config.yaml")),
		fmt.Sprintf("services/%s/ exists", name))
	check(fileExistsAt(run.ServiceComposeFile(dir, name)),
		fmt.Sprintf("services/%s/docker-compose.yml exists", name))

	svcCfg, _ := config.Load(config.ServiceConfigFile(dir, name))
	if svcCfg != nil {
		env, err := config.BuildEnv(rootConfigFile(), dir, name, sm)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: config error (%v)\n", err)
		}
		if env == nil {
			env = make(map[string]string)
		}
		for k, e := range svcCfg.Vars {
			if e.Required {
				check(env[k] != "", k+" is set")
			}
		}
		for k, e := range svcCfg.Secrets {
			isSet := sm != nil && sm.IsSet(name, k)
			if e.Required {
				check(isSet, k+" is set in keyring")
			}
		}
	}

	// ── Containers ────────────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Bold.Render("Containers"))

	dc, err := docker.New()
	if err != nil {
		fmt.Printf("  %s  Docker SDK unavailable: %v\n", styles.Warning.Render("!"), err)
	} else {
		defer func() { _ = dc.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		summaries, _ := dc.ServiceContainers(ctx, name)
		if len(summaries) == 0 {
			check(false, "no containers found")
		} else {
			for _, s := range summaries {
				check(s.State == containerStateRunning,
					fmt.Sprintf("%s %s", s.Name, stateLabel(s.State)))
			}
		}
	}

	// ── Routing ───────────────────────────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.Bold.Render("Routing"))

	mgr := caddy.New(dir)
	enabled, _ := mgr.IsEnabled(name)
	pubEnabled, _ := mgr.IsPublicEnabled(name)
	check(enabled || pubEnabled, "at least one Caddy route active")

	if !enabled && !pubEnabled && fix {
		// Check if caddy.conf exists so we can re-link it.
		caddyConf := filepath.Join(dir, "services", name, "caddy.conf")
		if fileExistsAt(caddyConf) {
			if err := mgr.Enable(name); err == nil {
				fmt.Printf("  %s  private route re-enabled\n", styles.Success.Render("✓"))
				pass = true
			}
		}
	}

	fmt.Println()
	if pass {
		fmt.Printf("  %s\n", styles.Success.Render("All checks passed."))
	} else {
		fmt.Printf("  %s\n", styles.Err.Render("Some checks failed."))
		if !fix {
			fmt.Printf("  %s\n", styles.Muted.Render("Run with --fix to auto-repair safe issues."))
		}
	}
	return pass
}

// hasResolvedExtension reports whether cfg has an extension that resolves
// to the given canonical name (handles name aliasing like yggdrasil → ygg).
func hasResolvedExtension(cfg *config.Config, canonicalName string) bool {
	if cfg == nil {
		return false
	}
	for _, ext := range cfg.Extensions {
		if config.ResolveExtension(ext) == canonicalName {
			return true
		}
	}
	return false
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorFixFlag, "fix", false, "Auto-repair safe issues (missing network, broken symlinks, missing dirs)")
	doctorCmd.Flags().BoolVar(&doctorAllFlag, "all", false, "Run doctor for all installed services")
	serviceDoctorCmd.Flags().BoolVar(&serviceDoctorFlags.all, "all", false, "Run doctor for all installed services")
	serviceDoctorCmd.Flags().BoolVar(&serviceDoctorFlags.fix, "fix", false, "Auto-repair safe issues")
	serviceCmd.AddCommand(serviceDoctorCmd)
}
