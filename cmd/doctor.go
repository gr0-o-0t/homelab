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
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/diagnostics"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/edge"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/swarm"
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

  homelab doctor          # environment, the agent's Caddy, routing
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
			for i := range svcs {
				svc := &svcs[i]
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
		if err := validName(args[0]); err != nil {
			return err
		}
		if !runServiceDoctorFor(dir, args[0], doctorFixFlag) {
			return fmt.Errorf("%s: some checks failed", args[0])
		}
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

	warns := renderCheckGroup(diagnostics.RunConfigChecks(cfgFile), &pass)
	configPass := pass
	infra := diagnostics.RunInfraChecks(dc)
	warns += renderCheckGroup(infra, &pass)

	// --fix: create the overlay network if that check failed, as `up` would.
	// Only that failure is repaired: any other keeps the run failing.
	netMissing, otherFail := false, false
	for _, r := range infra.Results {
		switch {
		case r.Status != diagnostics.StatusFail:
		case r.Name == "home-services network":
			netMissing = true
		default:
			otherFail = true
		}
	}
	if netMissing && doctorFixFlag && dc != nil {
		if err := spinner.Run("Creating overlay network 'home-services'…", func() error {
			return withDeployer(dir, false, func(ctx context.Context, d *swarm.Deployer) error {
				return d.EnsureNetwork(ctx)
			})
		}); err != nil {
			fmt.Printf("  %s  Could not create network: %v\n", styles.Warning.Render("!"), err)
		} else {
			fmt.Printf("  %s  Network 'home-services' created\n", styles.Success.Render("✓"))
			// The config checks above already ran; pass is only restored
			// when the network was the infrastructure's one failure and
			// the config checks passed.
			pass = !otherFail && configPass
		}
	}

	// The edge: the agent's Caddy answers, and the routes adapt.
	fmt.Printf("\n  %s\n", styles.Bold.Render("Edge"))
	if err := edge.Reachable(edge.AdminSocket(dir)); err != nil {
		fmt.Printf("  %s  Agent's Caddy not reachable: %v\n", styles.Err.Render("✗"), err)
		pass = false
	} else {
		fmt.Printf("  %s  Agent's Caddy answers on its admin socket\n", styles.Success.Render("✓"))
		var buf strings.Builder
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(dir, r).Validate(); err == nil {
			fmt.Printf("  %s  Caddy config valid\n", styles.Success.Render("✓"))
		} else {
			fmt.Printf("  %s  Caddy config valid: %v\n", styles.Err.Render("✗"), err)
			pass = false
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
		fmt.Printf("  %s\n\n", styles.Success.Render(passedSummary(warns)))
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
func renderCheckGroup(g diagnostics.CheckGroup, pass *bool) (warns int) {
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
			warns++
		}
	}
	return warns
}

// caddyRoutingCheck checks the routes directory, caddy/sites, and with --fix
// creates it and removes dangling symlinks: a config dir from before the
// static caddy.conf scheme was retired can still hold links into service
// directories.
func caddyRoutingCheck(dir string, fix bool, pass *bool) []diagnostics.CheckResult {
	var results []diagnostics.CheckResult
	for _, d := range []string{
		configgen.SitesDir(dir),
	} {
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
		cState := extLayerState(layer, dc)
		displayName := layer.Label()
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

	src, closeSrc := stateSource(dir)
	defer closeSrc()

	warns := renderCheckGroup(diagnostics.RunServiceConfigChecks(dir, name), &pass)
	warns += renderCheckGroup(diagnostics.RunServiceContainerChecks(name, src, desiredStopped(dir, name)), &pass)
	warns += renderCheckGroup(diagnostics.RunServiceRoutingChecks(dir, name), &pass)

	fmt.Println()
	if pass {
		fmt.Printf("  %s\n", styles.Success.Render(passedSummary(warns)))
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

// extLayerState is a layer's container state. cloudflared is a Swarm service,
// so the cf layer reports its task state (Status), never a container-name
// lookup; the others ask the Docker daemon.
func extLayerState(layer network.NetworkLayer, dc *docker.Client) string {
	if layer.Name() == cloudflaredLayer {
		return layer.Status().ContainerState
	}
	if dc == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return dc.ContainerState(ctx, layer.ContainerName())
}

// passedSummary is the closing line of a run with no failure: warnings are
// not failures, but they are not "all passed" either.
func passedSummary(warns int) string {
	switch warns {
	case 0:
		return "All checks passed."
	case 1:
		return "Passed with 1 warning."
	}
	return fmt.Sprintf("Passed with %d warnings.", warns)
}
