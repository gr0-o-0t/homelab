package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// Presentation for service listings: tables, the JSON form of a service, and
// the small formatters they share. Nothing here reaches for Docker or the
// filesystem — callers pass in what was already discovered.

func printServiceTable(svcs []service.Service, env map[string]string, wide bool) {
	if len(svcs) == 0 {
		fmt.Println(styles.Muted.Render("\n  No services found.\n"))
		return
	}

	fmt.Printf("\n  %s  %s\n\n",
		styles.Header.Render("Homelab Services"),
		styles.Muted.Render(fmt.Sprintf("%d services", len(svcs))),
	)

	fmt.Printf("  %s  %s  %s",
		styles.TableHeader.Render(styles.Width(styles.ColWidthName).Render("SERVICE")),
		styles.TableHeader.Render(styles.Width(12).Render("STATE")),
		styles.TableHeader.Render(styles.Width(styles.ColWidthExpose).Render("EXPOSURES")),
	)
	if wide {
		fmt.Printf("  %s  %s",
			styles.TableHeader.Render(styles.Width(styles.ColWidthPorts).Render("PORTS")),
			styles.TableHeader.Render("URL"),
		)
	}
	fmt.Println()
	fmt.Println(styles.Divider.Render("  " + strings.Repeat("─", styles.ColWidthName+12+styles.ColWidthExpose+6)))

	for _, svc := range svcs {
		name := styles.Width(styles.ColWidthName).Render(truncate(svc.Name, styles.ColWidthName-1))

		var stateCol string
		switch {
		case svc.Total == 0:
			stateCol = styles.Muted.Render("stopped")
		case svc.Running == svc.Total:
			stateCol = styles.Success.Render(fmt.Sprintf("%d/%d", svc.Running, svc.Total))
		default:
			stateCol = styles.Warning.Render(fmt.Sprintf("%d/%d", svc.Running, svc.Total))
		}

		// LAYERS column
		var layerTags string
		hasAnyLayer := svc.Enabled || svc.PublicEnabled || svc.HasTor || svc.HasI2P || svc.HasYgg
		if hasAnyLayer {
			var parts []string
			if svc.Enabled {
				parts = append(parts, styles.Success.Render("ts"))
			}
			if svc.PublicEnabled {
				parts = append(parts, styles.Primary.Render("cf"))
			}
			if svc.HasTor {
				parts = append(parts, styles.Accent.Render("tor"))
			}
			if svc.HasI2P {
				parts = append(parts, styles.Warning.Render("i2p"))
			}
			if svc.HasYgg {
				parts = append(parts, styles.Primary.Render("ygg"))
			}
			layerTags = strings.Join(parts, " ")
		}

		fmt.Printf("  %s  %s  %s", name, styles.Width(12).Render(stateCol), layerTags)
		if wide {
			var portsStr string
			if len(svc.HostPorts) > 0 {
				portsStr = styles.Width(styles.ColWidthPorts).Render(truncate(strings.Join(svc.HostPorts, ", "), styles.ColWidthPorts-1))
			} else {
				portsStr = styles.Width(styles.ColWidthPorts).Render(styles.Muted.Render("–"))
			}
			var ustr string
			if svc.Enabled && env["HOME_SUBDOMAIN"] != "" && env["DOMAIN"] != "" {
				ustr = styles.Muted.Render(fmt.Sprintf("https://%s.%s.%s", svc.Name, env["HOME_SUBDOMAIN"], env["DOMAIN"]))
			}
			fmt.Printf("  %s  %s", portsStr, ustr)
		}
		fmt.Println()
	}
	fmt.Println()
}

// discoverServices tries the Docker SDK first for live container data, then
// falls back to plain filesystem discovery if the daemon is unavailable.

// printPsTable renders a rich container table for `service ps`.
// Ports and Restart columns added alongside existing health/uptime/image columns.
func printPsTable(name string, summaries []docker.ContainerSummary, details []docker.ContainerDetail) {
	fmt.Printf("\n  %s %s\n\n",
		styles.Header.Render("Service:"),
		styles.Bold.Render(name),
	)

	const (
		wName    = 24
		wState   = 12
		wHealth  = 12
		wUptime  = 14
		wPorts   = 22
		wRestart = 8
	)

	fmt.Printf("  %s  %s  %s  %s  %s  %s  %s\n",
		styles.TableHeader.Render(styles.Width(wName).Render("CONTAINER")),
		styles.TableHeader.Render(styles.Width(wState).Render("STATE")),
		styles.TableHeader.Render(styles.Width(wHealth).Render("HEALTH")),
		styles.TableHeader.Render(styles.Width(wUptime).Render("UPTIME")),
		styles.TableHeader.Render(styles.Width(wPorts).Render("PORTS")),
		styles.TableHeader.Render(styles.Width(wRestart).Render("RESTART")),
		styles.TableHeader.Render("IMAGE"),
	)
	fmt.Println(styles.Divider.Render("  " + strings.Repeat("─", wName+wState+wHealth+wUptime+wPorts+wRestart+36)))

	for i, s := range summaries {
		cName := styles.Width(wName).Render(truncate(s.Name, wName-1))
		cState := styles.Width(wState).Render(styles.StateTag(s.State))
		cImage := styles.Muted.Render(truncate(s.Image, 36))

		var cHealth, cUptime, cPorts, cRestart string
		if details != nil && i < len(details) {
			d := details[i]
			cHealth = styles.Width(wHealth).Render(styles.HealthTag(d.Health))
			if d.State == containerStateRunning && !d.StartedAt.IsZero() {
				cUptime = styles.Width(wUptime).Render(
					styles.Success.Render("↑ " + formatUptime(time.Since(d.StartedAt))))
			} else if !d.FinishedAt.IsZero() && d.FinishedAt.Year() > 1 {
				cUptime = styles.Width(wUptime).Render(
					styles.Muted.Render("↓ " + formatUptime(time.Since(d.FinishedAt))))
			} else {
				cUptime = styles.Width(wUptime).Render(styles.Muted.Render("–"))
			}
			if len(d.Ports) > 0 {
				cPorts = styles.Width(wPorts).Render(truncate(strings.Join(d.Ports, ", "), wPorts-1))
			} else {
				cPorts = styles.Width(wPorts).Render(styles.Muted.Render("–"))
			}
			cRestart = styles.Width(wRestart).Render(fmt.Sprintf("%d", d.RestartCount))
		} else {
			cHealth = styles.Width(wHealth).Render(styles.Muted.Render("–"))
			cUptime = styles.Width(wUptime).Render(styles.Muted.Render(s.Status))
			cPorts = styles.Width(wPorts).Render(styles.Muted.Render("–"))
			cRestart = styles.Width(wRestart).Render(styles.Muted.Render("–"))
		}

		fmt.Printf("  %s  %s  %s  %s  %s  %s  %s\n", cName, cState, cHealth, cUptime, cPorts, cRestart, cImage)
	}
	fmt.Println()
}

// formatUptime converts a duration into a human-readable uptime string.

// formatUptime converts a duration into a human-readable uptime string.
func formatUptime(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// truncate shortens s to max chars, appending … if needed.

// truncate shortens s to max chars, appending … if needed.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// serviceJSON is the machine-readable shape of a service entry.

// serviceJSON is the machine-readable shape of a service entry.
type serviceJSON struct {
	Name               string   `json:"name"`
	Enabled            bool     `json:"enabled"`
	PublicEnabled      bool     `json:"publicEnabled"`
	HasCaddyConf       bool     `json:"hasCaddyConf"`
	HasPublicCaddyConf bool     `json:"hasPublicCaddyConf"`
	TorEnabled         bool     `json:"torEnabled"`
	I2PEnabled         bool     `json:"i2pEnabled"`
	YggEnabled         bool     `json:"yggEnabled"`
	HostPorts          []string `json:"hostPorts,omitempty"`
	Dir                string   `json:"dir"`
}

func printServiceJSON(svcs []service.Service) error {
	out := make([]serviceJSON, len(svcs))
	for i, s := range svcs {
		out[i] = serviceJSON{
			Name:               s.Name,
			Enabled:            s.Enabled,
			PublicEnabled:      s.PublicEnabled,
			HasCaddyConf:       s.HasCaddyConf,
			HasPublicCaddyConf: s.HasPublicCaddyConf,
			TorEnabled:         s.HasTor,
			I2PEnabled:         s.HasI2P,
			YggEnabled:         s.HasYgg,
			HostPorts:          s.HostPorts,
			Dir:                s.Dir,
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ── validation ────────────────────────────────────────────────────────────────

// validateService checks that services/<name>/ and its docker-compose.yml exist.

// buildServiceHint returns a styled list of known services for error messages.
func buildServiceHint(svcs []service.Service) string {
	if len(svcs) == 0 {
		return styles.Muted.Render("  (no services found in services/)")
	}
	var sb strings.Builder
	sb.WriteString(styles.Muted.Render("  Available services:"))
	for _, s := range svcs {
		sb.WriteString("\n    " + styles.Primary.Render(s.Name))
	}
	return sb.String()
}

// ── tab completion ────────────────────────────────────────────────────────────
