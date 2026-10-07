package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

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
	// One pill per core container, named with the same short tags the list
	// badges and `homelab status` use.
	pills := []string{m.corePill("caddy", m.core["caddy"])}
	for _, l := range m.layers {
		pills = append(pills, m.corePill(l.Name(), m.core[l.ContainerName()]))
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
	if m.help {
		right = renderHelp(bodyHeight, rightWidth)
	}

	sep := styles.PaneBorder.Render(strings.TrimRight(strings.Repeat("│\n", bodyHeight), "\n"))

	// Panes end in a newline and can run long; clamp each to exactly
	// bodyHeight or the frame grows past the terminal and the header scrolls
	// off the top.
	fit := func(s string, w int) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		if len(lines) > bodyHeight {
			lines = lines[:bodyHeight]
		}
		return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(bodyHeight).Render(strings.Join(lines, "\n"))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, fit(left, listInnerWidth), sep, fit(right, rightWidth))
}

// ── List pane ─────────────────────────────────────────────────────────────────

func (m Model) renderListPane(height int) string {
	visible := m.visibleServices()

	// Count installed vs catalog in the visible slice.
	installedCount, catalogCount := 0, 0
	for _, s := range visible {
		if isCore(&s) {
			continue
		}
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

	if isCore(&svc) {
		dot := styles.Dot(svc.Running == svc.Total && svc.Total > 0, true)
		ns := lipgloss.NewStyle().Width(installedNameW).Bold(true).Foreground(styles.ColText)
		return cursor + dot + " " + ns.Render("core") + " " +
			styles.Muted.Render(fmt.Sprintf("%d/%d", svc.Running, svc.Total))
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

	if isCore(svc) {
		return m.renderCoreDetail(svc, height, width)
	}
	if !svc.Installed {
		return m.renderCatalogDetail(svc, height, width)
	}
	return m.renderInstalledDetail(svc, height, width)
}

// renderCoreDetail shows each core container and what the core keys run.
func (m Model) renderCoreDetail(svc *service.Service, height, width int) string {
	var b strings.Builder
	w := width - 2
	tag := styles.Success.Render(fmt.Sprintf("● %d/%d running", svc.Running, svc.Total))
	if svc.Running < svc.Total {
		tag = styles.Warning.Render(fmt.Sprintf("● %d/%d running", svc.Running, svc.Total))
	}
	b.WriteString(" " + lipgloss.NewStyle().Width(w).Render(styles.Bold.Render("core stack")+"  "+tag) + "\n")
	b.WriteString(" " + styles.PaneBorder.Render(strings.Repeat("─", max(w, 1))) + "\n")

	b.WriteString("\n " + styles.PaneTitle.Render("Containers") + "\n")
	for _, c := range m.coreContainers() {
		state := m.core[c]
		st := styles.Muted.Render("not running")
		switch state {
		case "running":
			st = styles.Success.Render(state)
		case "":
		default:
			st = styles.Warning.Render(state)
		}
		fmt.Fprintf(&b, "  %s %s\n", lipgloss.NewStyle().Width(14).Render(c), st)
	}

	b.WriteString("\n " + styles.PaneTitle.Render("Actions") + "\n")
	for _, a := range [][2]string{
		{"u", "homelab up        create/start the core"},
		{"r", "homelab restart"},
		{"U", "homelab update    refresh files, pull, rebuild"},
		{"enter", "homelab logs -f   core logs"},
	} {
		fmt.Fprintf(&b, "  %s %s\n", lipgloss.NewStyle().Width(8).Render(key(a[0])), styles.Muted.Render(a[1]))
	}
	b.WriteString("  " + styles.Muted.Render("stop/down the core from a shell — it serves every route") + "\n")

	content := b.String()
	for strings.Count(content, "\n") < height {
		content += "\n"
	}
	return lipgloss.NewStyle().Width(width).Render(content)
}

func (m Model) renderCatalogDetail(svc *service.Service, height, width int) string {
	var b strings.Builder
	w := width - 2

	titleLine := styles.Bold.Render(svc.Name) + "  " + styles.Muted.Render("○ not installed")
	b.WriteString(" " + lipgloss.NewStyle().Width(w).Render(titleLine) + "\n")
	b.WriteString(" " + styles.PaneBorder.Render(strings.Repeat("─", w)) + "\n")

	b.WriteString("\n " + styles.Muted.Render("Bundled service — not yet installed.") + "\n\n")

	b.WriteString(" " + styles.PaneTitle.Render("Install") + "\n\n")
	fmt.Fprintf(&b, "  Press %s to install this service.\n", key("enter"))
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

	// Access: one row per configured layer, each address resolved by the
	// layer that owns that network — the same source `homelab status` reads.
	b.WriteString("\n " + styles.PaneTitle.Render("Access") + "\n")
	active := map[string]bool{}
	for _, n := range svc.ActiveLayers() {
		active[string(n)] = true
	}
	for _, l := range m.layers {
		tag := fmt.Sprintf("%-4s", l.Name())
		if !active[l.Name()] {
			fmt.Fprintf(&b, "  %s %s %s\n",
				styles.Muted.Render("○"), styles.Muted.Render(tag), styles.Muted.Render("off"))
			continue
		}
		for _, addr := range l.ServiceAddresses(svc.Name, env) {
			text := styles.Primary.Render(addr.URL)
			if addr.Note != "" {
				text = strings.TrimSpace(text + " " + styles.Muted.Render("("+addr.Note+")"))
			}
			fmt.Fprintf(&b, "  %s %s %s\n", styles.Success.Render("●"), tag, text)
		}
	}
	if len(m.layers) > 0 {
		b.WriteString("  " + key("e") + styles.Muted.Render(" enable  ") +
			key("d") + styles.Muted.Render(" disable") + "\n")
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
		hints = hintBar("enter", "keep filter", "esc", "clear", "backspace", "delete")

	case stateEnablePrompt, stateDisablePrompt:
		verb, all := "Enable", "all"
		if m.state == stateDisablePrompt {
			verb, all = "Disable", "all"
		}
		var pairs []string
		if svc := m.selectedService(); svc != nil {
			for _, c := range m.promptChoices(svc) {
				label := c.layer
				if c.layer == "ts" {
					label = "private"
				}
				pairs = append(pairs, c.key, label)
			}
			verb += " " + svc.Name
		}
		pairs = append(pairs, "a", all, "esc", "cancel")
		hints = styles.Warning.Render(verb+":  ") + hintBar(pairs...)

	case stateBusy:
		hints = styles.Primary.Render(m.spin.View() + " " + m.busyMsg)

	default:
		svc := m.selectedService()
		switch {
		case m.lastErr != "":
			hints = styles.Err.Render("✗ " + m.lastErr)
		case m.lastMsg != "":
			hints = styles.Success.Render("✓ " + m.lastMsg)
		case isCore(svc):
			hints = hintBar("u", "up", "r", "restart", "U", "update", "enter", "logs", "?", "help", "q", "quit")
		case svc != nil && !svc.Installed:
			hints = hintBar("enter", "install", "n", "new", "/", "filter", "?", "help", "q", "quit")
		case svc != nil:
			// Ordered by importance: the bar is clipped on narrow terminals.
			hints = hintBar("u", "up", "s", "stop", "r", "restart", "e", "enable", "d", "disable",
				"enter", "logs", "?", "help", "q", "quit", "/", "filter")
		default:
			hints = hintBar("n", "new", "/", "filter", "?", "help", "q", "quit")
		}
	}

	return lipgloss.NewStyle().
		Width(m.width).
		MaxHeight(1).
		Background(lipgloss.Color("#1E2030")).
		Padding(0, 1).
		Render(clipWidth(hints, m.width-2))
}

// helpKeys is the full keymap shown by '?'. Action names are the CLI
// commands they run, so what the dashboard does is what the docs say.
var helpKeys = [][2]string{
	{"", "Service"},
	{"u", "up       create + start"},
	{"s", "stop     keep containers"},
	{"r", "restart"},
	{"x", "down     remove containers"},
	{"U", "update   pull + recreate"},
	{"e", "enable   add a layer"},
	{"d", "disable  remove a layer"},
	{"enter", "logs · install"},
	{"", "General"},
	{"n", "new service"},
	{"/", "filter · esc clears"},
	{"R", "refresh (auto 5s)"},
	{"j/k ↑/↓", "move"},
	{"gg/G", "top / bottom"},
	{"ctrl+u/d", "half page up / down"},
	{"?", "close help"},
	{"q", "quit"},
}

func renderHelp(height, width int) string {
	var b strings.Builder
	b.WriteString(" " + styles.Bold.Render("Keys") + "\n")
	b.WriteString(" " + styles.PaneBorder.Render(strings.Repeat("─", max(width-2, 1))) + "\n")
	for _, h := range helpKeys {
		if h[0] == "" {
			b.WriteString("\n " + styles.PaneTitle.Render(h[1]) + "\n")
			continue
		}
		fmt.Fprintf(&b, "  %s %s\n",
			lipgloss.NewStyle().Width(10).Render(styles.Primary.Render(h[0])), h[1])
	}
	content := b.String()
	for strings.Count(content, "\n") < height {
		content += "\n"
	}
	return lipgloss.NewStyle().Width(width).MaxHeight(height).Render(content)
}

// hintBar renders key/label pairs as "[k] label  [k] label".
func hintBar(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, key(pairs[i])+" "+pairs[i+1])
	}
	return strings.Join(parts, "  ")
}

// clipWidth trims a styled string to w visible cells so the bar never wraps.
func clipWidth(s string, w int) string {
	if w <= 0 || lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
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
