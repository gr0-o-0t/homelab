package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// `homelab status <service>` — the per-service detail view, as opposed to the
// whole-stack overview in status.go.

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
			if layer, ok := extRegistry().Get(entry.key); ok {
				if addrs := layer.ServiceAddresses(name, env); len(addrs) > 0 {
					url = addressText(addrs[0])
				}
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
