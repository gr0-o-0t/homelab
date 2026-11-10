package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/groot/homelab/internal/configgen"
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
	for i := range svcs {
		s := &svcs[i]
		if s.Name == name {
			svc = &svcs[i]
			break
		}
	}
	if svc == nil {
		return fmt.Errorf("service %q not found", name)
	}

	running := svc.Running > 0
	priv, pub := svc.On("ts"), svc.On("cf")
	dot := styles.Dot(running, priv || pub)

	var access string
	switch {
	case priv && pub:
		access = styles.Success.Render("priv+pub")
	case priv:
		access = styles.Primary.Render("private")
	case pub:
		access = styles.Warning.Render("public")
	default:
		access = styles.Muted.Render("hidden")
	}

	var containerStatus string
	switch {
	case svc.StateUnknown:
		containerStatus = styles.Warning.Render("unknown (Docker/Swarm unavailable)")
	case svc.Total == 0:
		containerStatus = styles.Muted.Render("stopped")
	case svc.Running == svc.Total:
		containerStatus = styles.Success.Render(fmt.Sprintf("%d/%d running", svc.Running, svc.Total))
	default:
		containerStatus = styles.Warning.Render(fmt.Sprintf("%d/%d running", svc.Running, svc.Total))
	}

	var url string
	if priv && env["HOME_SUBDOMAIN"] != "" && env["DOMAIN"] != "" {
		url = fmt.Sprintf("https://%s.%s.%s", configgen.ServiceHost(dir, name), env["HOME_SUBDOMAIN"], env["DOMAIN"])
	}

	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Status: %s", name)))
	fmt.Printf("  %s  %s\n", dot, styles.Bold.Render(name))
	fmt.Printf("  %s  Access:  %s\n", styles.Muted.Render("↳"), access)
	fmt.Printf("  %s  State:   %s\n", styles.Muted.Render("↳"), containerStatus)
	if url != "" {
		fmt.Printf("  %s  URL:     %s\n", styles.Muted.Render("↳"), styles.Primary.Render(url))
	}
	if priv {
		fmt.Printf("  %s  Config:  %s\n", styles.Muted.Render("↳"),
			styles.Muted.Render(configgen.SitesFile(dir, name)))
	}

	// ── Network Exposure section ──────────────────────────────────────────
	fmt.Printf("\n  %s\n", styles.PaneTitle.Render("Network Exposure"))

	for _, layer := range extRegistry().All() {
		enabled := svc.On(layer.Name())
		icon := styles.Muted.Render("✗")
		if enabled {
			icon = styles.Success.Render("✓")
		}
		label := styles.Width(28).Render(styles.Bold.Render(layer.Label()))

		var url string
		if enabled {
			if addrs := layer.ServiceAddresses(name, env); len(addrs) > 0 {
				url = addressText(addrs[0])
			}
		}

		if url != "" {
			fmt.Printf("  %s  %s  %s\n", icon, label, styles.Primary.Render(url))
		} else {
			fmt.Printf("  %s  %s  %s\n", icon, label, styles.Muted.Render("—"))
		}
	}
	fmt.Println()

	// Container-level detail table: one row per task Swarm wants running,
	// with the node it runs on.
	if len(svc.Containers) > 0 {
		fmt.Printf("\n  %s\n", styles.PaneTitle.Render("Containers"))
		const (
			wCName  = 22
			wCState = 12
			wCNode  = 14
			wCPorts = 22
			wCUp    = 14
		)
		fmt.Printf("  %s  %s  %s  %s  %s  %s\n",
			styles.Width(wCName).Render(""),
			styles.TableHeader.Render(styles.Width(wCState).Render("STATE")),
			styles.TableHeader.Render(styles.Width(wCNode).Render("NODE")),
			styles.TableHeader.Render(styles.Width(wCPorts).Render("PORTS")),
			styles.TableHeader.Render(styles.Width(wCUp).Render("UPTIME")),
			styles.TableHeader.Render("IMAGE"),
		)
		for i := range svc.Containers {
			d := &svc.Containers[i]
			nameCol := styles.Width(wCName).Render(truncate(d.Name, wCName-1))
			stateCol := styles.Width(wCState).Render(mergedCoreState(d.State, d.Health))
			node := d.Node
			if node == "" {
				node = "–"
			}
			nodeCol := styles.Width(wCNode).Render(truncate(node, wCNode-1))
			portsCol := styles.Width(wCPorts).Render(styles.Muted.Render("–"))
			if len(d.Ports) > 0 {
				portsCol = styles.Width(wCPorts).Render(truncate(strings.Join(d.Ports, ", "), wCPorts-1))
			}
			uptimeCol := styles.Width(wCUp).Render(styles.Muted.Render("–"))
			if d.State == containerStateRunning && !d.StartedAt.IsZero() {
				uptimeCol = styles.Width(wCUp).Render(styles.Success.Render("↑ " + formatUptime(time.Since(d.StartedAt))))
			}
			imageCol := styles.Muted.Render(truncate(d.Image, 30))
			fmt.Printf("  %s  %s  %s  %s  %s  %s\n", nameCol, stateCol, nodeCol, portsCol, uptimeCol, imageCol)
		}
	}

	fmt.Println()
	return nil
}
