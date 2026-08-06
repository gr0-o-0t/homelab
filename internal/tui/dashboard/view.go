package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// ── constants ─────────────────────────────────────────────────────────────────

// View layer: every function here turns Model state into a string and touches
// nothing else — no Docker, no filesystem, no config.
//
// Split out of model.go, which held the state machine, the side-effecting
// commands and all of this in 1,337 lines. Rendering is the part that changes
// most often and the part that needs to stay obviously side-effect free.

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		m.renderBody(),
		m.renderStatusBar(),
	)
}

// ── Header ────────────────────────────────────────────────────────────────────

func (m Model) renderHeader() string {
	pills := []string{
		m.corePill("tailscale", m.core.tailscale),
		m.corePill("caddy", m.core.caddy),
	}
	if m.core.cloudflared != "" || m.isTunnelConfigured() {
		pills = append(pills, m.corePill("tunnel", m.core.cloudflared))
	}
	// Network layer pills from the registry
	for _, l := range m.layers {
		state := m.core.get(l.ContainerName())
		pills = append(pills, m.corePill(l.Label(), state))
	}
	right := strings.Join(pills, "  ")

	summary := m.serviceCountSummary()
	left := styles.Header.Render("homelab")
	if summary != "" {
		left = left + "  " + summary
	}

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	bar := left + strings.Repeat(" ", gap) + right

	return lipgloss.NewStyle().
		Width(m.width).
		Background(lipgloss.Color("#1E2030")).
		Padding(0, 1).
		Render(bar)
}

func (m Model) corePill(name, state string) string {
	switch state {
	case "running":
		return styles.Success.Render("●") + " " + styles.Muted.Render(name)
	case "":
		return styles.Muted.Render("○") + " " + styles.Muted.Render(name)
	default:
		return styles.Warning.Render("●") + " " + styles.Muted.Render(name)
	}
}

func (m Model) serviceCountSummary() string {
	var running, installed, available int
	for _, s := range m.services {
		if s.Installed {
			installed++
			if s.Running > 0 {
				running++
			}
		} else {
			available++
		}
	}
	var parts []string
	if running > 0 {
		parts = append(parts, styles.Success.Render(fmt.Sprintf("%d running", running)))
	}
	if installed > 0 {
		parts = append(parts, styles.Muted.Render(fmt.Sprintf("%d installed", installed)))
	}
	if available > 0 {
		parts = append(parts, styles.Muted.Render(fmt.Sprintf("%d available", available)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, styles.Muted.Render(" · "))
}

// ── Body ──────────────────────────────────────────────────────────────────────

func (m Model) renderBody() string {
	bodyHeight := m.height - headerLines - statusbarLines
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	rightWidth := m.width - leftPaneWidth
	if rightWidth < 10 {
		rightWidth = 10
	}

	left := m.renderListPane(bodyHeight)
	right := m.renderDetailPane(bodyHeight, rightWidth)

	sepStyle := styles.PaneBorder
	if m.focused == paneList {
		sepStyle = styles.PaneFocusBorder
	}
	sep := sepStyle.Render(strings.Repeat("│\n", bodyHeight))
	sep = strings.TrimRight(sep, "\n")

	return lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(listInnerWidth).Render(left),
		sep,
		lipgloss.NewStyle().Width(rightWidth).Render(right),
	)
}

// ── List pane ─────────────────────────────────────────────────────────────────

func (m Model) renderListPane(height int) string {
	visible := m.visibleServices()

	// Count installed vs catalog in the visible slice.
	installedCount, catalogCount := 0, 0
	for _, s := range visible {
		if s.Installed {
			installedCount++
		} else {
			catalogCount++
		}
	}

	// Available rows for service items = height - 1 (title)
	// - potential 1 for catalog separator.
	hasSeparator := m.filter == "" && catalogCount > 0
	itemRows := height - 1
	if hasSeparator {
		itemRows--
	}
	if itemRows < 1 {
		itemRows = 1
	}

	// Compute scroll offset so the selected item stays visible.
	// Keep cursor at ~1/3 from the top when scrolled past the window.
	scrollOffset := 0
	if m.cursor >= itemRows {
		targetRow := itemRows / 3
		scrollOffset = m.cursor - targetRow
		// Clamp so we don't scroll past the last item.
		if maxStart := len(visible) - itemRows; scrollOffset > maxStart {
			scrollOffset = maxStart
		}
	}

	var b strings.Builder

	// Title line.
	title := m.renderListTitle(installedCount, catalogCount)
	b.WriteString(lipgloss.NewStyle().Width(listInnerWidth).Render(title) + "\n")
	linesUsed := 1

	// Service rows, starting from scrollOffset. Only render up to itemRows items.
	// The separator is purely visual — it does not affect cursor indexing.
	separatorInserted := false
	rendered := 0
	for i := scrollOffset; i < len(visible) && rendered < itemRows; i++ {
		svc := visible[i]
		// Insert separator before first catalog entry (only when not filtering).
		if !svc.Installed && !separatorInserted && m.filter == "" {
			if linesUsed < height {
				b.WriteString(renderCatalogSeparator() + "\n")
				linesUsed++
				separatorInserted = true
			}
		}
		if linesUsed >= height {
			break
		}
		b.WriteString(m.renderListItem(svc, i == m.cursor) + "\n")
		linesUsed++
		rendered++
	}

	for linesUsed < height {
		b.WriteString("\n")
		linesUsed++
	}
	return b.String()
}

func (m Model) renderListTitle(installedCount, catalogCount int) string {
	if m.state == stateFilterInput {
		return styles.Warning.Render("/ ") + styles.Text.Render(m.filter) + styles.Muted.Render("_")
	}
	if m.filter != "" {
		visible := m.visibleServices()
		return styles.Muted.Render(fmt.Sprintf("/ %s  ", m.filter)) +
			styles.Muted.Render(fmt.Sprintf("%d/%d", len(visible), len(m.services)))
	}
	detail := fmt.Sprintf("%d installed", installedCount)
	if catalogCount > 0 {
		detail += fmt.Sprintf(" · %d available", catalogCount)
	}
	return styles.PaneTitle.Render("Services") + "  " + styles.Muted.Render(detail)
}

func renderCatalogSeparator() string {
	const label = " catalog "
	const indent = 2
	const dashLeft = 2
	dashRight := listInnerWidth - indent - dashLeft - len(label)
	if dashRight < 1 {
		dashRight = 1
	}
	line := strings.Repeat(" ", indent) +
		strings.Repeat("─", dashLeft) +
		label +
		strings.Repeat("─", dashRight)
	return styles.Muted.Render(line)
}

func (m Model) renderListItem(svc service.Service, selected bool) string {
	cursor := "  "
	if selected {
		cursor = styles.Primary.Render("▶ ")
	}

	if !svc.Installed {
		plus := styles.Muted.Render("+")
		ns := lipgloss.NewStyle().Width(catalogNameW).Foreground(styles.ColMuted)
		if selected {
			ns = ns.Foreground(styles.ColText)
		}
		return cursor + plus + " " + ns.Render(clip(svc.Name, catalogNameW))
	}

	running := svc.Running > 0
	exposed := svc.Enabled || svc.PublicEnabled
	dot := styles.Dot(running, exposed)

	ns := lipgloss.NewStyle().Width(installedNameW)
	if selected {
		ns = ns.Bold(true).Foreground(styles.ColText)
	} else {
		ns = ns.Foreground(styles.ColMuted)
	}
	name := ns.Render(clip(svc.Name, installedNameW))
	badge := exposureBadge(svc)

	return fmt.Sprintf("%s%s %s %s", cursor, dot, name, badge)
}

func exposureBadge(svc service.Service) string {
	w := lipgloss.NewStyle().Width(7)
	var active []string
	if svc.Enabled {
		active = append(active, "ts")
	}
	if svc.PublicEnabled {
		active = append(active, "cf")
	}
	if svc.HasTor {
		active = append(active, "tor")
	}
	if svc.HasI2P {
		active = append(active, "i2p")
	}
	if svc.HasYgg {
		active = append(active, "ygg")
	}
	if len(active) == 0 {
		return w.Foreground(styles.ColMuted).Render("       ")
	}
	if len(active) <= 2 {
		return w.Foreground(styles.ColSuccess).Render(strings.Join(active, "+"))
	}
	return w.Foreground(styles.ColSuccess).Render(fmt.Sprintf("%dlyrs", len(active)))
}

// ── Detail pane ───────────────────────────────────────────────────────────────

func (m Model) renderDetailPane(height, width int) string {
	svc := m.selectedService()
	if svc == nil {
		return lipgloss.NewStyle().Width(width).Height(height).
			Foreground(styles.ColMuted).
			Render("  No service selected")
	}

	if !svc.Installed {
		return m.renderCatalogDetail(svc, height, width)
	}
	return m.renderInstalledDetail(svc, height, width)
}

func (m Model) renderCatalogDetail(svc *service.Service, height, width int) string {
	var b strings.Builder
	w := width - 2

	titleLine := styles.Bold.Render(svc.Name) + "  " + styles.Muted.Render("○ not installed")
	b.WriteString(" " + lipgloss.NewStyle().Width(w).Render(titleLine) + "\n")
	b.WriteString(" " + styles.PaneBorder.Render(strings.Repeat("─", w)) + "\n")

	b.WriteString("\n " + styles.Muted.Render("Bundled service — not yet installed.") + "\n\n")

	b.WriteString(" " + styles.PaneTitle.Render("Install") + "\n\n")
	fmt.Fprintf(&b, "  Press %s to install this service.\n", key("i"))
	fmt.Fprintf(&b, "  Or run: %s\n\n", styles.Primary.Render("homelab add "+svc.Name))

	b.WriteString(" " + styles.Muted.Render("After installing:") + "\n")
	b.WriteString("  " + styles.Muted.Render(fmt.Sprintf("homelab setup %s", svc.Name)) + "\n")
	b.WriteString("  " + styles.Muted.Render(fmt.Sprintf("homelab up %s", svc.Name)) + "\n")
	b.WriteString("  " + styles.Muted.Render(fmt.Sprintf("homelab enable %s", svc.Name)) + "\n")

	content := b.String()
	for strings.Count(content, "\n") < height {
		content += "\n"
	}
	return lipgloss.NewStyle().Width(width).Render(content)
}

func (m Model) renderInstalledDetail(svc *service.Service, height, width int) string {
	var b strings.Builder
	w := width - 2

	// Title row
	var stateTag string
	switch {
	case svc.Running == svc.Total && svc.Total > 0:
		stateTag = styles.Success.Render(fmt.Sprintf("● %d/%d running", svc.Running, svc.Total))
	case svc.Total > 0:
		stateTag = styles.Warning.Render(fmt.Sprintf("● %d/%d running", svc.Running, svc.Total))
	default:
		stateTag = styles.Muted.Render("○ stopped")
	}
	titleLine := styles.Bold.Render(svc.Name) + "  " + stateTag
	b.WriteString(" " + lipgloss.NewStyle().Width(w).Render(titleLine) + "\n")
	b.WriteString(" " + styles.PaneBorder.Render(strings.Repeat("─", w)) + "\n")

	env := m.rootEnv()
	domain := env["DOMAIN"]
	homeSub := env["HOME_SUBDOMAIN"]

	// Access
	b.WriteString("\n " + styles.PaneTitle.Render("Access") + "\n")
	if svc.HasCaddyConf {
		if svc.Enabled {
			url := fmt.Sprintf("https://%s.%s.%s", svc.Name, homeSub, domain)
			fmt.Fprintf(&b, "  %s private   %s\n",
				styles.Success.Render("●"), styles.Primary.Render(url))
		} else {
			fmt.Fprintf(&b, "  %s private   %s\n",
				styles.Muted.Render("○"), styles.Muted.Render("not exposed"))
		}
	}
	if svc.HasPublicCaddyConf {
		if svc.PublicEnabled {
			url := fmt.Sprintf("https://%s.%s", svc.Name, domain)
			fmt.Fprintf(&b, "  %s public    %s\n",
				styles.Success.Render("●"), styles.Primary.Render(url))
		} else {
			fmt.Fprintf(&b, "  %s public    %s\n",
				styles.Muted.Render("○"), styles.Muted.Render("not exposed"))
		}
	}

	// Extension layer addresses, each resolved by the layer that owns the
	// network — the same source `homelab status` reads, so the two can no
	// longer disagree about where a service lives.
	for _, name := range svc.ActiveLayers() {
		if name == "ts" || name == "cf" {
			continue // rendered above with their own icons
		}
		layer, ok := m.layerByName(name)
		if !ok {
			continue
		}
		for _, addr := range layer.ServiceAddresses(svc.Name, env) {
			text := addr.URL
			if addr.Note != "" {
				text = strings.TrimSpace(text + " (" + addr.Note + ")")
			}
			fmt.Fprintf(&b, "  %s %-9s %s\n",
				styles.Accent.Render("●"), name, styles.Primary.Render(text))
		}
	}

	// Containers
	if len(svc.Containers) > 0 {
		b.WriteString("\n " + styles.PaneTitle.Render("Containers") + "\n")
		for i := range svc.Containers {
			c := &svc.Containers[i]
			cName := clip(c.Name, 20)
			stateStyle := styles.Muted
			switch c.State {
			case "running":
				stateStyle = styles.Success
			case "restarting":
				stateStyle = styles.Warning
			}
			stateStr := stateStyle.Render(c.State)
			var extra string
			if m.containerDetails != nil && i < len(m.containerDetails) {
				d := m.containerDetails[i]
				health := "–"
				if d.Health != "" {
					health = styles.HealthTag(d.Health)
				}
				ports := ""
				if len(d.Ports) > 0 {
					ports = " " + styles.Muted.Render(clip(strings.Join(d.Ports, ", "), 28))
				}
				extra = fmt.Sprintf("  %s  %s", health, ports)
			}
			fmt.Fprintf(&b, "  %s  %s%s\n",
				lipgloss.NewStyle().Width(20).Render(cName), stateStr, extra)
		}
	}

	// Log tail
	if len(m.logLines) > 0 && m.logSvcName == svc.Name {
		b.WriteString("\n " + styles.PaneTitle.Render("Logs") + "\n")
		logWidth := w - 2
		for _, line := range m.logLines {
			if line == "" {
				continue
			}
			b.WriteString("  " + styles.Muted.Render(clip(stripAnsi(line), logWidth)) + "\n")
		}
	}

	content := b.String()
	for strings.Count(content, "\n") < height {
		content += "\n"
	}
	return lipgloss.NewStyle().Width(width).Render(content)
}

// ── Status bar ────────────────────────────────────────────────────────────────

func (m Model) renderStatusBar() string {
	var hints string

	switch m.state {
	case stateFilterInput:
		hints = styles.Muted.Render("[enter/esc] done  [backspace] delete")

	case stateEnablePrompt:
		hints = styles.Warning.Render("Expose:  ") +
			key("p") + " private  " +
			key("P") + " public  " +
			key("b") + " both  " +
			key("esc") + " cancel"

	case stateDisablePrompt:
		hints = styles.Warning.Render("Hide:  ") +
			key("p") + " private  " +
			key("P") + " public  " +
			key("b") + " both  " +
			key("esc") + " cancel"

	case stateBusy:
		hints = styles.Primary.Render(m.spin.View() + " " + m.busyMsg)

	default:
		if m.lastErr != "" {
			hints = styles.Err.Render("✗ " + clip(m.lastErr, m.width-4))
		} else if m.lastMsg != "" {
			hints = styles.Success.Render("✓ " + m.lastMsg)
		} else {
			svc := m.selectedService()
			if svc != nil && !svc.Installed {
				hints = key("i") + " install  " +
					key("n") + " new  " +
					key("/") + " filter  " +
					key("R") + " refresh  " +
					key("j") + "↓  " +
					key("k") + "↑  " +
					key("q") + " quit"
			} else {
				hints = key("u") + " start  " +
					key("x") + " stop  " +
					key("r") + " restart  " +
					key("e") + " expose  " +
					key("d") + " hide  " +
					key("l") + " logs  " +
					key("n") + " new  " +
					key("tab") + " pane  " +
					key("/") + " filter  " +
					key("j") + "↓  " +
					key("k") + "↑  " +
					key("gg") + " top  " +
					key("G") + " bot  " +
					key("q") + " quit"
			}
		}
	}

	return lipgloss.NewStyle().
		Width(m.width).
		Background(lipgloss.Color("#1E2030")).
		Padding(0, 1).
		Render(hints)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func key(k string) string {
	return styles.Muted.Render("[") + styles.Primary.Render(k) + styles.Muted.Render("]")
}

func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		// len(s) is a byte count, always >= rune count for UTF-8, so passing
		// here guarantees the rune count is also <= n — safe to return as-is.
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// stripAnsi removes ANSI escape sequences from log output.

// stripAnsi removes ANSI escape sequences from log output.
func stripAnsi(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEsc {
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
				inEsc = false
			}
			continue
		}
		if c == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ── tea.Cmd functions ─────────────────────────────────────────────────────────
