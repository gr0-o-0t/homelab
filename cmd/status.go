package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/diagnostics"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:     "status [service]",
	Aliases: []string{"ps"},
	Short:   "Show homelab status overview",
	Long: `Display the status of the core stack (Tailscale, Caddy, network extensions)
and every installed service.

With a service argument, show status for that specific service.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runStatus,
}

var statusCheckFlag bool

func runStatus(_ *cobra.Command, args []string) error {
	dir := configDir()
	cfgFile := rootConfigFile()
	cfg, _ := config.Load(cfgFile)
	env := buildEnv(dir, "")

	// Per-service status
	if len(args) > 0 {
		return runServiceStatus(dir, args[0], env)
	}

	fmt.Printf("\n%s\n\n", styles.Header.Render("Homelab Status"))

	// ── Core Stack ────────────────────────────────────────────────────────────
	fmt.Printf("  %s\n", styles.Bold.Render("Core Stack"))

	// Core services to display: always-on (caddy) + registry layers.
	// Build a combined list from the registry, then prepend caddy.
	type coreEntry struct {
		Name string
		Ext  string // extension ID; empty for always-on core
		show func(env map[string]string) bool
	}

	var entries []coreEntry

	// Caddy is the sole core — always show.
	entries = append(entries, coreEntry{Name: "caddy", Ext: ""})

	// Layers from registry (tailscale, cf, tor, i2p, ygg, ipfs)
	for _, layer := range extRegistry.All() {
		ename := layer.Name()
		// Special: CF shows also when CF_TUNNEL_TOKEN is set even if not enabled
		if ename == "cf" {
			entries = append(entries, coreEntry{
				Name: layer.ContainerName(),
				Ext:  ename,
				show: func(e map[string]string) bool { return e["CF_TUNNEL_TOKEN"] != "" },
			})
		} else {
			entries = append(entries, coreEntry{
				Name: layer.ContainerName(),
				Ext:  ename,
			})
		}
	}

	// ── Core table header ─────────────────────────────────────────────────────
	// Gather health info for core containers
	// Core containers all belong to compose project "core" (compose file at
	// core/docker-compose.yml), so we query by project "core" and match back
	// to core entries by container name.
	healthByProject := make(map[string]string)
	dc, dcErr := docker.New()
	if dcErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer func() { _ = dc.Close() }()

		summaries, err := dc.ServiceContainers(ctx, "core")
		if err == nil && len(summaries) > 0 {
			details, err := dc.InspectContainers(ctx, summaries)
			if err == nil {
				for i := range details {
					// Match by container name (e.g. "caddy", "tailscale")
					// to the core entry's Name (layer.ContainerName()).
					healthByProject[details[i].Name] = details[i].Health
				}
			}
		}
	}

	// Compute tailscale IP/FQDN once for the table
	tsIP, _ := tailscaleIP()
	tsFQDN, _ := tailscaleFQDN()

	fmt.Printf("  %s  %s  %s  %s\n",
		styles.TableHeader.Render(styles.Width(styles.ColWidthName).Render("SERVICE")),
		styles.TableHeader.Render(styles.Width(12).Render("STATE")),
		styles.TableHeader.Render(styles.Width(24).Render("IP/URL")),
		styles.TableHeader.Render(styles.Width(10).Render("CONFIG")),
	)
	divLen := styles.ColWidthName + 12 + 24 + 10 + 6
	fmt.Println(styles.Divider.Render("  " + strings.Repeat("─", divLen)))

	var (
		inactiveExts []string
		coreRunning  bool
	)

	for _, c := range entries {
		state := containerStatus(c.Name)

		// Core services (ext=="") always show. Extensions only show when
		// enabled in config, the container exists, or a check passes.
		isExt := c.Ext != ""
		extEnabled := isExt && cfg != nil && cfg.HasExtension(c.Ext)
		extConfigured := c.show != nil && c.show(env)
		if isExt && state == "not found" && !extEnabled && !extConfigured {
			continue
		}

		var icon string
		switch state {
		case containerStateRunning:
			icon = styles.Success.Render("✓")
			if !isExt {
				coreRunning = true
			}
		case "not found":
			icon = styles.Err.Render("✗")
		default:
			icon = styles.Warning.Render("!")
		}

		// IP/URL column
		var ipURL string
		if c.Name == "tailscale" && state == containerStateRunning {
			if tsFQDN != "" {
				ipURL = styles.Primary.Render(tsFQDN)
			} else if tsIP != "" {
				ipURL = styles.Primary.Render(tsIP)
			} else {
				ipURL = styles.Muted.Render("–")
			}
		} else {
			ipURL = styles.Muted.Render("–")
		}

		// CONFIG column
		var configTag string
		if c.Ext == "" {
			configTag = styles.Muted.Render("always")
		} else if extEnabled {
			configTag = styles.Success.Render("enabled")
		} else if extConfigured {
			configTag = styles.Warning.Render("configured")
		} else {
			configTag = styles.Muted.Render("–")
		}

		merged := mergedCoreState(state, healthByProject[c.Name])
		fmt.Printf("  %s  %s  %s  %s\n",
			styles.Width(styles.ColWidthName).Render(icon+" "+styles.Bold.Render(truncate(c.Name, styles.ColWidthName-3))),
			styles.Width(12).Render(merged),
			styles.Width(24).Render(ipURL),
			styles.Width(10).Render(configTag),
		)

		// Track extensions that are configured (e.g. token set) but not enabled.
		if isExt && state != containerStateRunning && extConfigured && !extEnabled {
			inactiveExts = append(inactiveExts, c.Ext)
		}
	}
	fmt.Println()

	// ── Footer hints (before services, so they show even with no services) ────
	var hints []string

	if !coreRunning {
		hints = append(hints, fmt.Sprintf("Run %s to start the core stack",
			styles.Primary.Render("homelab start")))
	}

	if len(inactiveExts) > 0 {
		for _, ext := range inactiveExts {
			hints = append(hints, fmt.Sprintf("Run %s to add %s",
				styles.Primary.Render("homelab ext enable "+ext),
				config.ExtensionLabel(ext)))
		}
	}

	if len(hints) > 0 {
		fmt.Printf("  %s\n", styles.Muted.Render(strings.Repeat("─", 50)))
		for _, h := range hints {
			fmt.Printf("  %s  %s\n", styles.Muted.Render("→"), h)
		}
		fmt.Println()
	}

	// ── Services ──────────────────────────────────────────────────────────────
	svcs, err := discoverServices(dir)
	if err != nil {
		svcs = nil
	}

	if len(svcs) == 0 {
		fmt.Printf("  %s  No services installed.\n", styles.Muted.Render("!"))
		fmt.Printf("  %s  Run %s from the catalog, or %s to scaffold a new one.\n\n",
			styles.Muted.Render("→"),
			styles.Primary.Render("homelab add <name>"),
			styles.Primary.Render("homelab new"))
		return nil
	}

	privateCount, publicCount, runningCount := 0, 0, 0
	var torCount, i2pCount, yggCount, ipfsCount int
	for _, s := range svcs {
		if s.Enabled {
			privateCount++
		}
		if s.PublicEnabled {
			publicCount++
		}
		if s.HasTor {
			torCount++
		}
		if s.HasI2P {
			i2pCount++
		}
		if s.HasYgg {
			yggCount++
		}
		if s.HasIPFS {
			ipfsCount++
		}
		if s.Running > 0 {
			runningCount++
		}
	}

	summaryLine := fmt.Sprintf("%d installed / %d running", len(svcs), runningCount)
	if torCount > 0 || i2pCount > 0 || yggCount > 0 || ipfsCount > 0 {
		summaryLine += fmt.Sprintf(" / ts:%d cf:%d tor:%d i2p:%d ygg:%d ipfs:%d",
			privateCount, publicCount, torCount, i2pCount, yggCount, ipfsCount)
	} else {
		summaryLine += fmt.Sprintf(" / %d private / %d public", privateCount, publicCount)
	}
	fmt.Printf("  %s  %s\n\n", styles.Bold.Render("Services"), styles.Muted.Render(summaryLine))

	// Gather health for service containers
	svcHealth := make(map[string]string)
	sd, sdErr := docker.New()
	if sdErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		for _, s := range svcs {
			containers, err := sd.ServiceContainers(ctx, s.Name)
			if err != nil || len(containers) == 0 {
				continue
			}
			details, err := sd.InspectContainers(ctx, containers)
			if err == nil && len(details) > 0 {
				svcHealth[s.Name] = details[0].Health
			}
		}
		_ = sd.Close()
	}

	// Table header
	fmt.Printf("  %s  %s  %s  %s  %s\n",
		styles.TableHeader.Render(styles.Width(styles.ColWidthName).Render("SERVICE")),
		styles.TableHeader.Render(styles.Width(12).Render("STATE")),
		styles.TableHeader.Render(styles.Width(styles.ColWidthPorts).Render("PORTS")),
		styles.TableHeader.Render(styles.Width(styles.ColWidthNetwork).Render("NETWORK")),
		styles.TableHeader.Render("URL"),
	)
	divLen = styles.ColWidthName + 12 + styles.ColWidthPorts + styles.ColWidthNetwork + 50 + 8
	fmt.Println(styles.Divider.Render("  " + strings.Repeat("─", divLen)))

	for _, svc := range svcs {
		name := styles.Width(styles.ColWidthName).Render(truncate(svc.Name, styles.ColWidthName-1))

		// STATE column — merged state + health
		stateCol := styles.Width(12).Render(mergedState(svc, svcHealth[svc.Name]))

		// PORTS column — always visible
		var portsStr string
		if len(svc.HostPorts) > 0 {
			portsStr = styles.Width(styles.ColWidthPorts).Render(truncate(strings.Join(svc.HostPorts, ", "), styles.ColWidthPorts-1))
		} else {
			portsStr = styles.Width(styles.ColWidthPorts).Render(styles.Muted.Render("–"))
		}

		// Collect active exposures with URLs
		type expRow struct {
			tag string
			url string
		}
		var rows []expRow

		if svc.Enabled {
			u := configgen.LayerDisplayURL("private", svc.Name, env)
			rows = append(rows, expRow{styles.Success.Render("ts"), u})
		}
		if svc.PublicEnabled {
			u := configgen.LayerDisplayURL("cf", svc.Name, env)
			rows = append(rows, expRow{styles.Primary.Render("cf"), u})
		}
		if svc.HasTor {
			u := torOnionAddress(svc.Name)
			if u == "" {
				u = configgen.LayerDisplayURL("tor", svc.Name, env)
			} else {
				u = "http://" + u
			}
			rows = append(rows, expRow{styles.Accent.Render("tor"), u})
		}
		if svc.HasI2P {
			u := configgen.LayerDisplayURL("i2p", svc.Name, env)
			rows = append(rows, expRow{styles.Warning.Render("i2p"), u})
		}
		if svc.HasYgg {
			u := configgen.LayerDisplayURL("ygg", svc.Name, env)
			rows = append(rows, expRow{styles.Primary.Render("ygg"), u})
		}
		if svc.HasIPFS {
			u := configgen.LayerDisplayURL("ipfs", svc.Name, env)
			rows = append(rows, expRow{styles.Muted.Render("ipfs"), u})
		}

		if len(rows) == 0 {
			// No exposures — single row with dash in NETWORK column
			fmt.Printf("  %s  %s  %s  %s  %s\n",
				name,
				stateCol,
				portsStr,
				styles.Width(styles.ColWidthNetwork).Render(styles.Muted.Render("–")),
				"",
			)
		} else {
			// First exposure on summary row
			fmt.Printf("  %s  %s  %s  %s  %s\n",
				name,
				stateCol,
				portsStr,
				styles.Width(styles.ColWidthNetwork).Render(rows[0].tag),
				styles.Muted.Render(rows[0].url),
			)
			// Remaining exposures as sub-rows (empty SERVICE/STATE/PORTS)
			for _, r := range rows[1:] {
				fmt.Printf("  %s  %s  %s  %s  %s\n",
					styles.Width(styles.ColWidthName).Render(""),
					styles.Width(12).Render(""),
					styles.Width(styles.ColWidthPorts).Render(""),
					styles.Width(styles.ColWidthNetwork).Render(r.tag),
					styles.Muted.Render(r.url),
				)
			}
		}
	}

	// ── Diagnostics (--check) ─────────────────────────────────────────────────────
	if statusCheckFlag {
		dc, _ := docker.New()
		if dc != nil {
			defer func() { _ = dc.Close() }()
		}
		var pass = true
		fmt.Println()
		groups := []diagnostics.CheckGroup{
			diagnostics.RunConfigChecks(cfgFile),
			diagnostics.RunInfraChecks(dc),
		}
		for _, g := range groups {
			if len(g.Results) == 0 {
				continue
			}
			fmt.Printf("\n  %s\n", styles.Bold.Render(g.Title))
			for _, r := range g.Results {
				switch r.Status {
				case diagnostics.StatusPass:
					fmt.Printf("  %s  %s\n", styles.Success.Render("✓"), r.Message)
				case diagnostics.StatusFail:
					fmt.Printf("  %s  %s\n", styles.Err.Render("✗"), r.Message)
					pass = false
				case diagnostics.StatusWarn:
					fmt.Printf("  %s  %s\n", styles.Warning.Render("!"), r.Message)
				}
			}
		}
		fmt.Println()
		if !pass {
			fmt.Printf("  %s\n", styles.Err.Render("Some checks failed."))
		}
	}

	return nil
}

// runServiceStatus shows status for a single service with container detail table.
func runServiceStatus(dir, name string, env map[string]string) error {
	svcs, err := discoverServices(dir)
	if err != nil {
		return err
	}

	var svc *service.Service
	for i, s := range svcs {
		if s.Name == name {
			svc = &svcs[i]
			break
		}
	}
	if svc == nil {
		return fmt.Errorf("service %q not found", name)
	}

	running := svc.Running > 0
	dot := styles.Dot(running, svc.Enabled || svc.PublicEnabled)

	var access string
	switch {
	case svc.Enabled && svc.PublicEnabled:
		access = styles.Success.Render("priv+pub")
	case svc.Enabled:
		access = styles.Primary.Render("private")
	case svc.PublicEnabled:
		access = styles.Warning.Render("public")
	default:
		access = styles.Muted.Render("hidden")
	}

	var containerStatus string
	switch {
	case svc.Total == 0:
		containerStatus = styles.Muted.Render("stopped")
	case svc.Running == svc.Total:
		containerStatus = styles.Success.Render(fmt.Sprintf("%d/%d running", svc.Running, svc.Total))
	default:
		containerStatus = styles.Warning.Render(fmt.Sprintf("%d/%d running", svc.Running, svc.Total))
	}

	var url string
	if svc.Enabled && env["HOME_SUBDOMAIN"] != "" && env["DOMAIN"] != "" {
		url = fmt.Sprintf("https://%s.%s.%s", name, env["HOME_SUBDOMAIN"], env["DOMAIN"])
	}

	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Status: %s", name)))
	fmt.Printf("  %s  %s\n", dot, styles.Bold.Render(name))
	fmt.Printf("  %s  Access:  %s\n", styles.Muted.Render("↳"), access)
	fmt.Printf("  %s  State:   %s\n", styles.Muted.Render("↳"), containerStatus)
	if url != "" {
		fmt.Printf("  %s  URL:     %s\n", styles.Muted.Render("↳"), styles.Primary.Render(url))
	}
	if svc.Enabled && svc.HasCaddyConf {
		fmt.Printf("  %s  Config:  %s\n", styles.Muted.Render("↳"), styles.Muted.Render(filepath.Join(svc.Dir, "caddy.conf")))
	}

	// ── Network Exposure section ──────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.PaneTitle.Render("Network Exposure"))

	layerEntries := []struct {
		key     string // configgen layer key
		label   string
		enabled bool
	}{
		{"private", "Tailnet", svc.Enabled},
		{"cf", "Cloudflare", svc.PublicEnabled},
		{"tor", "Tor", svc.HasTor}, // URL resolved below — use torOnionAddress when running
		{"i2p", "I2P", svc.HasI2P},
		{"ygg", "Yggdrasil", svc.HasYgg},
	}

	for _, entry := range layerEntries {
		icon := styles.Muted.Render("✗")
		if entry.enabled {
			icon = styles.Success.Render("✓")
		}
		label := styles.Width(14).Render(styles.Bold.Render(entry.label))

		var url string
		if entry.enabled {
			if entry.key == "tor" {
				u := torOnionAddress(name)
				if u == "" {
					url = configgen.LayerDisplayURL("tor", name, env)
				} else {
					url = "http://" + u
				}
			} else {
				url = configgen.LayerDisplayURL(entry.key, name, env)
			}
		}

		if url != "" {
			fmt.Printf("  %s  %s  %s\n", icon, label, styles.Primary.Render(url))
		} else {
			fmt.Printf("  %s  %s  %s\n", icon, label, styles.Muted.Render("—"))
		}
	}
	fmt.Println()

	// Container-level detail table
	dc, dcErr := docker.New()
	if dcErr == nil {
		defer func() { _ = dc.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		summaries, err := dc.ServiceContainers(ctx, name)
		if err == nil && len(summaries) > 0 {
			details, _ := dc.InspectContainers(ctx, summaries)
			fmt.Printf("\n  %s\n", styles.PaneTitle.Render("Containers"))
			const (
				wCName   = 22
				wCState  = 12
				wCHealth = 12
				wCPorts  = 22
				wCUp     = 14
				wCImg    = 30
			)
			fmt.Printf("  %s  %s  %s  %s  %s\n",
				styles.Width(wCName).Render(""),
				styles.TableHeader.Render(styles.Width(wCState).Render("STATE")),
				styles.TableHeader.Render(styles.Width(wCPorts).Render("PORTS")),
				styles.TableHeader.Render(styles.Width(wCUp).Render("UPTIME")),
				styles.TableHeader.Render("IMAGE"),
			)
			for i, s := range summaries {
				nameCol := styles.Width(wCName).Render(truncate(s.Name, wCName-1))
				var stateCol, portsCol, uptimeCol, imageCol string
				if details != nil && i < len(details) {
					d := details[i]
					stateCol = styles.Width(wCState).Render(mergedCoreState(s.State, d.Health))
					if len(d.Ports) > 0 {
						portsCol = styles.Width(wCPorts).Render(truncate(strings.Join(d.Ports, ", "), wCPorts-1))
					} else {
						portsCol = styles.Width(wCPorts).Render(styles.Muted.Render("–"))
					}
					if d.State == containerStateRunning && !d.StartedAt.IsZero() {
						uptimeCol = styles.Width(wCUp).Render(styles.Success.Render("↑ " + formatUptime(time.Since(d.StartedAt))))
					} else if !d.FinishedAt.IsZero() && d.FinishedAt.Year() > 1 {
						uptimeCol = styles.Width(wCUp).Render(styles.Muted.Render("↓ " + formatUptime(time.Since(d.FinishedAt))))
					} else {
						uptimeCol = styles.Width(wCUp).Render(styles.Muted.Render("–"))
					}
					imageCol = styles.Muted.Render(truncate(d.Image, 30))
				} else {
					stateCol = styles.Width(wCState).Render(styles.StateTag(s.State))
					portsCol = styles.Width(wCPorts).Render(styles.Muted.Render("–"))
					uptimeCol = styles.Width(wCUp).Render(styles.Muted.Render(s.Status))
					imageCol = styles.Muted.Render(truncate(s.Image, 30))
				}
				fmt.Printf("  %s  %s  %s  %s  %s\n", nameCol, stateCol, portsCol, uptimeCol, imageCol)
			}
		}
	}

	fmt.Println()
	return nil
}

// tailscaleFQDN returns the Tailscale node FQDN.
func tailscaleFQDN() (string, bool) {
	out, err := exec.Command(
		"docker", "exec", "tailscale",
		"tailscale", "status", "--self", "--json",
	).Output()
	if err != nil {
		return "", false
	}
	var self struct{ DNSName string }
	if json.Unmarshal(out, &self) != nil || self.DNSName == "" {
		return "", false
	}
	return strings.TrimSuffix(self.DNSName, "."), true
}

// mergedCoreState returns a styled state string that merges container state
// with Docker healthcheck status for core table entries.
func mergedCoreState(state, health string) string {
	switch {
	case state != "running":
		return styles.StateTag(state)
	case health == "healthy":
		return styles.Success.Render("healthy")
	case health == "starting":
		return styles.Warning.Render("starting")
	case health == "unhealthy":
		return styles.Err.Render("unhealthy")
	default:
		return styles.Success.Render("running")
	}
}

// mergedState returns a styled state string that merges container state
// with Docker healthcheck status. Used by the services sub-table.
//
// Returns one of: healthy, running, starting, unhealthy, restarting,
// stopped, partial (N/M).
func mergedState(svc service.Service, health string) string {
	// Check container-level states first for states that Running count can't distinguish.
	for _, c := range svc.Containers {
		if c.State == "restarting" {
			return styles.Warning.Render("restarting")
		}
	}

	switch {
	case svc.Total == 0:
		return styles.Muted.Render("stopped")
	case svc.Running == 0:
		return styles.Muted.Render("stopped")
	case svc.Running < svc.Total:
		return styles.Warning.Render(fmt.Sprintf("partial (%d/%d)", svc.Running, svc.Total))
	case health == "healthy":
		return styles.Success.Render("healthy")
	case health == "starting":
		return styles.Warning.Render("starting")
	case health == "unhealthy":
		return styles.Err.Render("unhealthy")
	default:
		return styles.Success.Render("running")
	}
}

func init() {
	statusCmd.Flags().BoolVar(&statusCheckFlag, "check", false, "Run health checks inline")
}
