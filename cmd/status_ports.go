package cmd

import (
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// withPorts sets each service's declared ports, with what its exposure.yaml
// enables and where each answers (configgen.PortStatuses).
func withPorts(dir string, svcs []service.Service, env map[string]string) {
	for i := range svcs {
		svcs[i].Ports = configgen.PortStatuses(dir, svcs[i].Name, env)
	}
}

// portsColumn is the PORTS column of `homelab status`: the enabled ports
// ("2222/tcp, https"), then any host-published ones; "–" when none.
func portsColumn(svc *service.Service) string {
	labels := append(configgen.EnabledPortLabels(svc.Ports), svc.HostPorts...)
	if len(labels) == 0 {
		return styles.Width(styles.ColWidthPorts).Render(styles.Muted.Render("–"))
	}
	return styles.Width(styles.ColWidthPorts).Render(truncate(strings.Join(labels, ", "), styles.ColWidthPorts-1))
}

// printPortsSection is the "Ports" section of `homelab status <svc>`: each
// declared port by the name `enable --ports=` takes, where it answers, and
// whether it is enabled.
func printPortsSection(svc *service.Service) {
	if len(svc.Ports) == 0 {
		return
	}
	fmt.Printf("\n  %s\n", styles.PaneTitle.Render("Ports"))
	const wName, wLabel = 12, 14
	for _, p := range svc.Ports {
		name := p.Name
		if name == "" {
			name = "(routes)" // caddy.routes.conf: one site, not selectable
		}
		target := p.Target
		if target == "" {
			target = "–"
		}
		icon, state := styles.Success.Render("✓"), ""
		if !p.Enabled {
			icon, state = styles.Muted.Render("✗"), styles.Muted.Render("  not enabled")
		}
		if p.Note != "" {
			state = styles.Muted.Render("  " + p.Note)
		}
		fmt.Printf("  %s  %s  %s  %s → :%d%s\n", icon,
			styles.Width(wName).Render(styles.Bold.Render(truncate(name, wName-1))),
			styles.Width(wLabel).Render(truncate(p.Label, wLabel-1)),
			styles.Primary.Render(target), p.Port, state)
	}
}
