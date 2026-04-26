package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check homelab health and configuration",
	Long: `Runs a series of health checks on the homelab environment:

  • Config directory present and config.yaml valid
  • Required vars and secrets populated
  • Docker daemon running
  • home-services network present
  • /dev/net/tun device available
  • Core containers (tailscale, caddy) running
  • Caddy config valid
  • Tailscale connected

Pass --fix to automatically repair safe issues (missing network, broken symlinks).`,
	RunE: runDoctor,
}

func runDoctor(_ *cobra.Command, _ []string) error {
	dir := configDir()
	cfgFile := rootConfigFile()

	fmt.Printf("\n%s\n\n", styles.Header.Render("Homelab Health Check"))

	sm, _ := secrets.Open()

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
	caddyConfDPub := filepath.Join(dir, "caddy", "conf.d-pub")

	for _, d := range []string{caddyConfD, caddyConfDPub} {
		rel, _ := filepath.Rel(dir, d)
		if _, err := os.Stat(d); os.IsNotExist(err) {
			check(false, rel+" directory present")
			if doctorFixFlag {
				if mkErr := os.MkdirAll(d, 0o755); mkErr == nil {
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
	check(tsState == "running", fmt.Sprintf("tailscale container %s", stateLabel(tsState)))

	caddyState := containerStatus("caddy")
	caddyRunning := caddyState == "running"
	check(caddyRunning, fmt.Sprintf("caddy container %s", stateLabel(caddyState)))

	if caddyRunning {
		var buf strings.Builder
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		valErr := caddy.NewWithRunner(dir, r).Validate()
		check(valErr == nil, "Caddy config valid")
	}

	if tsState == "running" {
		ip, connected := tailscaleIP()
		if connected {
			check(true, fmt.Sprintf("Tailscale connected (%s)", ip))
		} else {
			check(false, "Tailscale not connected")
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

	sm, _ := secrets.Open()

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
		env, _ := config.BuildEnv(rootConfigFile(), dir, name, sm)
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
				check(s.State == "running",
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

func init() {
	doctorCmd.Flags().BoolVar(&doctorFixFlag, "fix", false, "Auto-repair safe issues (missing network, broken symlinks, missing dirs)")
	serviceDoctorCmd.Flags().BoolVar(&serviceDoctorFlags.all, "all", false, "Run doctor for all installed services")
	serviceDoctorCmd.Flags().BoolVar(&serviceDoctorFlags.fix, "fix", false, "Auto-repair safe issues")
	serviceCmd.AddCommand(serviceDoctorCmd)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func fileExistsAt(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func dockerDaemonUp() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func containerStatus(name string) string {
	out, err := exec.Command(
		"docker", "inspect", "--format={{.State.Status}}", name,
	).Output()
	if err != nil {
		return "not found"
	}
	return strings.TrimSpace(string(out))
}

func stateLabel(state string) string {
	switch state {
	case "running":
		return styles.Success.Render("running")
	case "not found":
		return styles.Err.Render("not found")
	default:
		return styles.Warning.Render(state)
	}
}

func tailscaleIP() (string, bool) {
	out, err := exec.Command(
		"docker", "exec", "tailscale", "tailscale", "ip", "-4",
	).Output()
	if err != nil {
		return "", false
	}
	ip := strings.TrimSpace(string(out))
	return ip, ip != ""
}

// removeBrokenSymlinks scans dir for symlinks whose targets no longer exist.
// When fix is true it removes them and returns the count removed; otherwise it
// just returns the count of broken links found.
func removeBrokenSymlinks(dir string, fix bool) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		target, err := os.Readlink(path)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			count++
			if fix {
				_ = os.Remove(path)
			}
		}
	}
	return count
}
