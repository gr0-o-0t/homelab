package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

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

	for i := range svcs {
		svc := &svcs[i]
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
		var parts []string
		for _, l := range svc.ActiveLayers() {
			parts = append(parts, layerTag(l))
		}
		layerTags := strings.Join(parts, " ")

		fmt.Printf("  %s  %s  %s", name, styles.Width(12).Render(stateCol), layerTags)
		if wide {
			var portsStr string
			if len(svc.HostPorts) > 0 {
				portsStr = styles.Width(styles.ColWidthPorts).Render(truncate(strings.Join(svc.HostPorts, ", "), styles.ColWidthPorts-1))
			} else {
				portsStr = styles.Width(styles.ColWidthPorts).Render(styles.Muted.Render("–"))
			}
			var ustr string
			if svc.On("ts") && env["HOME_SUBDOMAIN"] != "" && env["DOMAIN"] != "" {
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
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// serviceJSON is the machine-readable shape of a service entry.
type serviceJSON struct {
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	PublicEnabled bool     `json:"publicEnabled"`
	TorEnabled    bool     `json:"torEnabled"`
	I2PEnabled    bool     `json:"i2pEnabled"`
	YggEnabled    bool     `json:"yggEnabled"`
	HostPorts     []string `json:"hostPorts,omitempty"`
	Dir           string   `json:"dir"`
}

func printServiceJSON(svcs []service.Service) error {
	out := make([]serviceJSON, len(svcs))
	for i := range svcs {
		s := &svcs[i]
		out[i] = serviceJSON{
			Name:          s.Name,
			Enabled:       s.On("ts"),
			PublicEnabled: s.On("cf"),
			TorEnabled:    s.On("tor"),
			I2PEnabled:    s.On("i2p"),
			YggEnabled:    s.On("ygg"),
			HostPorts:     s.HostPorts,
			Dir:           s.Dir,
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
	for i := range svcs {
		s := &svcs[i]
		sb.WriteString("\n    " + styles.Primary.Render(s.Name))
	}
	return sb.String()
}

// ── tab completion ────────────────────────────────────────────────────────────
