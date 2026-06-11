package cmd

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/groot/homelab/internal/config"
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
		fmt.Printf("  %s  %s  %s\n",
			icon,
			styles.Width(styles.ColWidthName).Render(styles.Bold.Render(c.Name)),
			styles.StateTag(state))

		// Track extensions that are configured (e.g. token set) but not enabled.
		if isExt && state != containerStateRunning && extConfigured && !extEnabled {
			inactiveExts = append(inactiveExts, c.Ext)
		}
	}

	// Tailscale IP + FQDN
	if state := containerStatus("tailscale"); state == containerStateRunning {
		ip, _ := tailscaleIP()
		if ip != "" {
			fmt.Printf("  %s  Tailscale IP:  %s\n", styles.Muted.Render("↳"), styles.Primary.Render(ip))
		}
		fqdn, _ := tailscaleFQDN()
		if fqdn != "" {
			fmt.Printf("  %s  FQDN:          %s\n", styles.Muted.Render("↳"), styles.Primary.Render(fqdn))
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
	for _, s := range svcs {
		if s.Enabled {
			privateCount++
		}
		if s.PublicEnabled {
			publicCount++
		}
		if s.Running > 0 {
			runningCount++
		}
	}

	fmt.Printf("  %s  %s\n\n",
		styles.Bold.Render("Services"),
		styles.Muted.Render(fmt.Sprintf(
			"%d installed / %d running / %d private / %d public",
			len(svcs), runningCount, privateCount, publicCount)),
	)

	for _, svc := range svcs {
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
			containerStatus = styles.Success.Render(fmt.Sprintf("%d running", svc.Running))
		default:
			containerStatus = styles.Warning.Render(fmt.Sprintf("%d/%d running", svc.Running, svc.Total))
		}

		var url string
		if svc.Enabled && env["HOME_SUBDOMAIN"] != "" && env["DOMAIN"] != "" {
			url = fmt.Sprintf("https://%s.%s.%s", svc.Name, env["HOME_SUBDOMAIN"], env["DOMAIN"])
		}

		fmt.Printf("  %s  %s  %s  %s\n", dot, styles.Width(styles.ColWidthName).Render(styles.Bold.Render(svc.Name)), access, containerStatus)
		if url != "" {
			fmt.Printf("  %s  %s\n", styles.Muted.Render("   "), styles.Muted.Render(url))
		}
	}

	return nil
}

// runServiceStatus shows status for a single service.
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
