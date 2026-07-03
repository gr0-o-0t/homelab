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
	"github.com/groot/homelab/internal/diagnostics"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
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

	var pass = true

	dc, dcErr := docker.New()
	if dcErr != nil {
		fmt.Printf("  %s  Docker SDK unavailable: %v\n", styles.Warning.Render("!"), dcErr)
	}
	if dc != nil {
		defer func() { _ = dc.Close() }()
	}

	renderCheckGroup(diagnostics.RunConfigChecks(cfgFile), &pass)
	renderCheckGroup(diagnostics.RunInfraChecks(dc), &pass)

	// --fix: create Docker network if missing
	if !pass && doctorFixFlag && dc != nil {
		if err := spinner.Run("Creating Docker network 'home-services'…", func() error {
			return run.Default().DockerNetworkCreate("home-services")
		}); err != nil {
			fmt.Printf("  %s  Could not create network: %v\n", styles.Warning.Render("!"), err)
		} else {
			fmt.Printf("  %s  Network 'home-services' created\n", styles.Success.Render("✓"))
			pass = true
		}
	}

	renderCheckGroup(diagnostics.RunCoreStackChecks(dc, dir), &pass)

	// Core stack extras — Caddy config validate + Tailscale connectivity
	if dc != nil {
		caddyState := dc.ContainerState(context.Background(), "caddy")
		if caddyState == containerStateRunning {
			var buf strings.Builder
			r := &run.Commander{Stdout: &buf, Stderr: &buf}
			if caddy.NewWithRunner(dir, r).Validate() == nil {
				fmt.Printf("  %s  Caddy config valid\n", styles.Success.Render("✓"))
			} else {
				fmt.Printf("  %s  Caddy config valid\n", styles.Err.Render("✗"))
				pass = false
			}
		}
		tsState := dc.ContainerState(context.Background(), "tailscale")
		if tsState == containerStateRunning {
			ip, ok := tailscaleIP()
			if ok {
				fmt.Printf("  %s  Tailscale connected (%s)\n", styles.Success.Render("✓"), styles.Primary.Render(ip))
			} else {
				fmt.Printf("  %s  Tailscale not connected\n", styles.Err.Render("✗"))
				pass = false
			}
		}
	}

	routingResults := caddyRoutingCheck(dir, doctorFixFlag, &pass)
	if len(routingResults) > 0 {
		fmt.Printf("\n  %s\n", styles.Bold.Render("Caddy routing"))
		for _, r := range routingResults {
			switch r.Status {
			case diagnostics.StatusPass:
				fmt.Printf("  %s  %s\n", styles.Success.Render("✓"), r.Message)
			case diagnostics.StatusFail:
				fmt.Printf("  %s  %s\n", styles.Err.Render("✗"), r.Message)
				pass = false
			}
		}
	}

	renderExtensionChecks(cfgFile, dc, &pass)

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

// renderCheckGroup prints a CheckGroup with the same ✓/✗/! format as the original doctor.
func renderCheckGroup(g diagnostics.CheckGroup, pass *bool) {
	fmt.Printf("\n  %s\n", styles.Bold.Render(g.Title))
	for _, r := range g.Results {
		switch r.Status {
		case diagnostics.StatusPass:
			fmt.Printf("  %s  %s\n", styles.Success.Render("✓"), r.Message)
		case diagnostics.StatusFail:
			fmt.Printf("  %s  %s\n", styles.Err.Render("✗"), r.Message)
			if pass != nil {
				*pass = false
			}
		case diagnostics.StatusWarn:
			fmt.Printf("  %s  %s\n", styles.Warning.Render("!"), r.Message)
		}
	}
}

// caddyRoutingCheck checks Caddy conf.d directories and broken symlinks.
// Returns results and handles --fix repair.
func caddyRoutingCheck(dir string, fix bool, pass *bool) []diagnostics.CheckResult {
	var results []diagnostics.CheckResult
	caddyConfD := filepath.Join(dir, "caddy", "conf.d")
	caddyConfDPub := filepath.Join(dir, "caddy", "conf.d-cf")

	for _, d := range []string{caddyConfD, caddyConfDPub} {
		rel, _ := filepath.Rel(dir, d)
		if _, err := os.Stat(d); os.IsNotExist(err) {
			results = append(results, diagnostics.CheckResult{
				Name: rel + " dir", Status: diagnostics.StatusFail, Message: rel + " dir present",
			})
			if pass != nil {
				*pass = false
			}
			if fix {
				if mkErr := os.MkdirAll(d, 0o750); mkErr == nil {
					fmt.Printf("  %s  %s created\n", styles.Success.Render("✓"), rel)
				} else {
					fmt.Printf("  %s  Could not create %s: %v\n", styles.Warning.Render("!"), rel, mkErr)
				}
			}
		} else {
			results = append(results, diagnostics.CheckResult{
				Name: rel + " dir", Status: diagnostics.StatusPass, Message: rel + " dir present",
			})
			brokenCount := removeBrokenSymlinks(d, fix)
			if brokenCount > 0 {
				fmt.Printf("  %s  %s: removed %d broken symlink(s)\n",
					styles.Success.Render("✓"), rel, brokenCount)
			}
		}
	}
	return results
}

// renderExtensionChecks iterates the registry and renders extension container states.
func renderExtensionChecks(cfgFile string, dc *docker.Client, pass *bool) {
	cfg, _ := config.Load(cfgFile)
	if cfg == nil {
		return
	}

	var results []diagnostics.CheckResult
	for _, layer := range extRegistry().All() {
		name := layer.Name()
		if !hasResolvedExtension(cfg, name) {
			continue
		}
		cName := layer.ContainerName()
		var cState string
		if dc != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			cState = dc.ContainerState(ctx, cName)
			cancel()
		} else {
			cState = ""
		}
		displayName := config.ExtensionLabel(name)
		if cState == containerStateRunning {
			results = append(results, diagnostics.CheckResult{
				Name: displayName, Status: diagnostics.StatusPass,
				Message: fmt.Sprintf("%s (%s) container running", displayName, cName),
			})
		} else {
			results = append(results, diagnostics.CheckResult{
				Name: displayName, Status: diagnostics.StatusWarn,
				Message: fmt.Sprintf("%s (%s) container %s", displayName, cName, stateLabel(cState)),
			})
			if pass != nil {
				*pass = false
			}
		}
	}

	if len(results) > 0 {
		fmt.Printf("\n  %s\n", styles.Bold.Render("Network Extensions"))
		for _, r := range results {
			switch r.Status {
			case diagnostics.StatusPass:
				fmt.Printf("  %s  %s\n", styles.Success.Render("✓"), r.Message)
			case diagnostics.StatusWarn:
				fmt.Printf("  %s  %s\n", styles.Err.Render("✗"), r.Message)
				if pass != nil {
					*pass = false
				}
			}
		}
	}
}

func runServiceDoctorFor(dir, name string, fix bool) bool {
	fmt.Printf("\n%s %s\n\n",
		styles.Header.Render("Service Health:"),
		styles.Bold.Render(name))

	pass := true

	dc, dcErr := docker.New()
	if dcErr != nil {
		fmt.Printf("  %s  Docker SDK unavailable: %v\n", styles.Warning.Render("!"), dcErr)
	}
	if dc != nil {
		defer func() { _ = dc.Close() }()
	}

	renderCheckGroup(diagnostics.RunServiceConfigChecks(dir, name), &pass)
	renderCheckGroup(diagnostics.RunServiceContainerChecks(name, dc), &pass)
	renderCheckGroup(diagnostics.RunServiceRoutingChecks(dir, name), &pass)

	// --fix auto-repair for Caddy routes (private only — matches original)
	if !pass && fix {
		mgr := caddy.New(dir)
		enabled, _ := mgr.IsEnabled(name)
		if !enabled {
			caddyConf := filepath.Join(dir, "services", name, "caddy.conf")
			if fileExistsAt(caddyConf) {
				if err := mgr.Enable(name); err == nil {
					fmt.Printf("  %s  private route re-enabled\n", styles.Success.Render("✓"))
					pass = true
				}
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

}
