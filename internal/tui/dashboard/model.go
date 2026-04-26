// Package dashboard implements the full-screen homelab dashboard TUI.
// Layout: header bar | left service list | right detail pane | status bar.
package dashboard

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// ── constants ─────────────────────────────────────────────────────────────────

const (
	leftPaneWidth  = 40 // full left column width (including separator)
	listInnerWidth = 38 // usable characters inside the left pane
	headerLines    = 1
	statusbarLines = 1
	logTailLines   = 10
	coreRefreshSec = 5
	logRefreshSec  = 4

	// Name column widths inside the list. Derived from listInnerWidth.
	//   installed item: cursor(2) + dot(1) + space(1) + name + space(1) + badge(4) = 9 + name
	//   catalog  item:  cursor(2) + plus(1) + space(1) + name                      = 4 + name
	installedNameW = listInnerWidth - 9 // 29
	catalogNameW   = listInnerWidth - 4 // 34
)

// ── state machine ─────────────────────────────────────────────────────────────

type dashState int

const (
	stateNormal dashState = iota
	stateBusy
	stateEnablePrompt
	stateDisablePrompt
	stateFilterInput
)

type pane int

const (
	paneList pane = iota
	paneDetail
)

// ── messages ──────────────────────────────────────────────────────────────────

type (
	refreshedMsg  struct{ services []service.Service }
	opDoneMsg     struct{ msg string }
	opErrMsg      struct{ err error; output string }
	coreStatusMsg struct{ ts, caddy, cloudflared string }
	logTailMsg    struct{ svcName string; lines []string }
	coreTickMsg   struct{}
	logTickMsg    struct{}
)

// ── core status ───────────────────────────────────────────────────────────────

type coreStatus struct {
	tailscale   string
	caddy       string
	cloudflared string
}

// ── EnvBuilderFn ──────────────────────────────────────────────────────────────

// EnvBuilderFn returns the docker compose environment map for a service name.
type EnvBuilderFn func(svcName string) map[string]string

// ── model ─────────────────────────────────────────────────────────────────────

// Model is the Bubble Tea model for the full-screen dashboard.
type Model struct {
	// layout
	width, height int
	focused       pane

	// state machine
	state   dashState
	spin    spinner.Model
	busyMsg string
	lastMsg string
	lastErr string

	// service list
	repoRoot     string
	dc           *docker.Client
	services     []service.Service
	catalogNames []string
	cursor       int
	filter       string
	buildEnv     EnvBuilderFn

	// core health header
	core coreStatus

	// detail pane log tail
	logLines   []string
	logSvcName string

	// exit signals
	SelectedForLogs    string
	SelectedForNew     bool
	SelectedForInstall string // catalog service name chosen for installation
}

// New constructs the dashboard Model.
// catalogNames lists all names from the embedded service catalog; services not
// yet installed appear in the list as available-to-install stubs.
func New(repoRoot string, dc *docker.Client, services []service.Service, catalogNames []string, buildEnv EnvBuilderFn) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Primary

	return Model{
		repoRoot:     repoRoot,
		dc:           dc,
		services:     services,
		catalogNames: catalogNames,
		buildEnv:     buildEnv,
		spin:         sp,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		refreshCmd(m.repoRoot, m.dc, m.catalogNames),
		coreRefreshCmd(m.dc),
		m.spin.Tick,
		coreTickCmd(),
		logTickCmd(),
	)
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case tea.KeyMsg:
		m, cmds = m.handleKey(msg, cmds)

	case refreshedMsg:
		m.services = msg.services
		m.uiIdle()
		// Clamp cursor if the list shrank.
		if visible := m.visibleServices(); m.cursor >= len(visible) && len(visible) > 0 {
			m.cursor = len(visible) - 1
		}
		cmds = append(cmds, m.fetchLogsCmd())

	case opDoneMsg:
		m.lastMsg = msg.msg
		m.lastErr = ""
		return m, tea.Batch(
			refreshCmd(m.repoRoot, m.dc, m.catalogNames),
			m.spin.Tick,
		)

	case opErrMsg:
		m.state = stateNormal
		m.busyMsg = ""
		m.lastMsg = ""
		if msg.output != "" {
			m.lastErr = clip(strings.TrimSpace(msg.output), 120)
		} else {
			m.lastErr = msg.err.Error()
		}

	case coreStatusMsg:
		m.core = coreStatus{
			tailscale:   msg.ts,
			caddy:       msg.caddy,
			cloudflared: msg.cloudflared,
		}

	case logTailMsg:
		if msg.svcName == m.selectedName() {
			m.logLines = msg.lines
			m.logSvcName = msg.svcName
		}

	case coreTickMsg:
		cmds = append(cmds, coreRefreshCmd(m.dc), coreTickCmd())

	case logTickMsg:
		cmds = append(cmds, m.fetchLogsCmd(), logTickCmd())

	case spinner.TickMsg:
		if m.state == stateBusy {
			var sCmd tea.Cmd
			m.spin, sCmd = m.spin.Update(msg)
			cmds = append(cmds, sCmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m Model) handleKey(msg tea.KeyMsg, cmds []tea.Cmd) (Model, []tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, append(cmds, tea.Quit)
	}

	switch m.state {

	// ── filter input ──────────────────────────────────────────────────────────
	case stateFilterInput:
		switch msg.String() {
		case "esc", "enter":
			m.state = stateNormal
		case "backspace":
			if len(m.filter) > 0 {
				m.filter = m.filter[:len(m.filter)-1]
			}
		default:
			if len(msg.Runes) == 1 {
				m.filter += string(msg.Runes)
			}
		}
		m.cursor = 0

	// ── route selection prompts ───────────────────────────────────────────────
	case stateEnablePrompt, stateDisablePrompt:
		switch msg.String() {
		case "esc":
			m.state = stateNormal
		case "p":
			svc := m.selectedService()
			if svc == nil {
				break
			}
			if m.state == stateEnablePrompt {
				m.busyOp(fmt.Sprintf("Enabling private route for %s…", svc.Name))
				cmds = append(cmds, privateEnableCmd(m.repoRoot, svc.Name), m.spin.Tick)
			} else {
				m.busyOp(fmt.Sprintf("Disabling private route for %s…", svc.Name))
				cmds = append(cmds, privateDisableCmd(m.repoRoot, svc.Name), m.spin.Tick)
			}
		case "P":
			svc := m.selectedService()
			if svc == nil {
				break
			}
			if m.state == stateEnablePrompt {
				env := m.rootEnv()
				if env["CF_TUNNEL_TOKEN"] == "" {
					m.state = stateNormal
					m.lastErr = "public exposure requires CF_TUNNEL_TOKEN — run `homelab setup`"
					break
				}
				m.busyOp(fmt.Sprintf("Enabling public route for %s…", svc.Name))
				cmds = append(cmds, publicEnableCmd(m.repoRoot, svc.Name), m.spin.Tick)
			} else {
				m.busyOp(fmt.Sprintf("Disabling public route for %s…", svc.Name))
				cmds = append(cmds, publicDisableCmd(m.repoRoot, svc.Name), m.spin.Tick)
			}
		case "b":
			svc := m.selectedService()
			if svc == nil {
				break
			}
			if m.state == stateEnablePrompt {
				env := m.rootEnv()
				if env["CF_TUNNEL_TOKEN"] == "" {
					m.state = stateNormal
					m.lastErr = "public exposure requires CF_TUNNEL_TOKEN — run `homelab setup`"
					break
				}
				m.busyOp(fmt.Sprintf("Enabling private + public for %s…", svc.Name))
				cmds = append(cmds, bothEnableCmd(m.repoRoot, svc.Name), m.spin.Tick)
			} else {
				m.busyOp(fmt.Sprintf("Disabling all routes for %s…", svc.Name))
				cmds = append(cmds, bothDisableCmd(m.repoRoot, svc.Name), m.spin.Tick)
			}
		}

	// ── normal mode ───────────────────────────────────────────────────────────
	case stateNormal:
		switch msg.String() {
		case "q":
			return m, append(cmds, tea.Quit)

		case "tab", "shift+tab":
			if m.focused == paneList {
				m.focused = paneDetail
			} else {
				m.focused = paneList
			}

		case "up", "k":
			if m.focused == paneList {
				if m.cursor > 0 {
					m.cursor--
					cmds = append(cmds, m.fetchLogsCmd())
				}
			}

		case "down", "j":
			if m.focused == paneList {
				if visible := m.visibleServices(); m.cursor < len(visible)-1 {
					m.cursor++
					cmds = append(cmds, m.fetchLogsCmd())
				}
			}

		case "/":
			m.state = stateFilterInput
			m.filter = ""
			m.cursor = 0

		case "e":
			if svc := m.selectedService(); svc != nil && svc.Installed {
				m.lastMsg, m.lastErr = "", ""
				m.state = stateEnablePrompt
			}

		case "d":
			if svc := m.selectedService(); svc != nil && svc.Installed {
				m.lastMsg, m.lastErr = "", ""
				m.state = stateDisablePrompt
			}

		case "u":
			if svc := m.selectedService(); svc != nil && svc.Installed {
				m.busyOp(fmt.Sprintf("Starting %s…", svc.Name))
				cmds = append(cmds, upCmd(m.repoRoot, svc.Name, m.buildEnv), m.spin.Tick)
			}

		case "x":
			if svc := m.selectedService(); svc != nil && svc.Installed {
				m.busyOp(fmt.Sprintf("Stopping %s…", svc.Name))
				cmds = append(cmds, downCmd(m.repoRoot, svc.Name, m.buildEnv), m.spin.Tick)
			}

		case "r":
			if svc := m.selectedService(); svc != nil && svc.Installed {
				m.busyOp(fmt.Sprintf("Restarting %s…", svc.Name))
				cmds = append(cmds, restartCmd(m.repoRoot, svc.Name, m.buildEnv), m.spin.Tick)
			}

		case "l":
			if svc := m.selectedService(); svc != nil && svc.Installed {
				m.SelectedForLogs = svc.Name
				return m, append(cmds, tea.Quit)
			}

		case "i":
			if svc := m.selectedService(); svc != nil && !svc.Installed {
				m.SelectedForInstall = svc.Name
				return m, append(cmds, tea.Quit)
			}

		case "n":
			m.SelectedForNew = true
			return m, append(cmds, tea.Quit)

		case "R":
			m.busyOp("Refreshing…")
			cmds = append(cmds, refreshCmd(m.repoRoot, m.dc, m.catalogNames), m.spin.Tick)
		}
	}

	return m, cmds
}

// ── View ──────────────────────────────────────────────────────────────────────

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

	var b strings.Builder

	// Title line.
	title := m.renderListTitle(installedCount, catalogCount)
	b.WriteString(lipgloss.NewStyle().Width(listInnerWidth).Render(title) + "\n")
	linesUsed := 1

	// Service rows, with a separator injected before the first catalog-only entry.
	// The separator is purely visual — it does not affect cursor indexing.
	separatorInserted := false
	for i, svc := range visible {
		if linesUsed >= height {
			break
		}
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
	w := lipgloss.NewStyle().Width(4)
	switch {
	case svc.Enabled && svc.PublicEnabled:
		return w.Foreground(styles.ColSuccess).Render("both")
	case svc.Enabled:
		return w.Foreground(styles.ColPrimary).Render("priv")
	case svc.PublicEnabled:
		return w.Foreground(styles.ColAccent).Render("pub")
	default:
		return w.Foreground(styles.ColMuted).Render("    ")
	}
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
	b.WriteString(fmt.Sprintf("  Press %s to install this service.\n", key("i")))
	b.WriteString(fmt.Sprintf("  Or run: %s\n\n", styles.Primary.Render("homelab service add "+svc.Name)))

	b.WriteString(" " + styles.Muted.Render("After installing:") + "\n")
	b.WriteString("  " + styles.Muted.Render(fmt.Sprintf("homelab service setup %s", svc.Name)) + "\n")
	b.WriteString("  " + styles.Muted.Render(fmt.Sprintf("homelab service up %s", svc.Name)) + "\n")
	b.WriteString("  " + styles.Muted.Render(fmt.Sprintf("homelab service enable %s --private", svc.Name)) + "\n")

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
	pubSub := env["PUB_SUBDOMAIN"]
	if pubSub == "" {
		pubSub = "pub"
	}

	// Access
	b.WriteString("\n " + styles.PaneTitle.Render("Access") + "\n")
	if svc.HasCaddyConf {
		if svc.Enabled {
			url := fmt.Sprintf("https://%s.%s.%s", svc.Name, homeSub, domain)
			b.WriteString(fmt.Sprintf("  %s private   %s\n",
				styles.Success.Render("●"), styles.Primary.Render(url)))
		} else {
			b.WriteString(fmt.Sprintf("  %s private   %s\n",
				styles.Muted.Render("○"), styles.Muted.Render("not exposed")))
		}
	}
	if svc.HasPublicCaddyConf {
		if svc.PublicEnabled {
			url := fmt.Sprintf("https://%s.%s.%s", svc.Name, pubSub, domain)
			b.WriteString(fmt.Sprintf("  %s public    %s\n",
				styles.Success.Render("●"), styles.Primary.Render(url)))
		} else {
			b.WriteString(fmt.Sprintf("  %s public    %s\n",
				styles.Muted.Render("○"), styles.Muted.Render("not exposed")))
		}
	}

	// Containers
	if len(svc.Containers) > 0 {
		b.WriteString("\n " + styles.PaneTitle.Render("Containers") + "\n")
		for _, c := range svc.Containers {
			stateStyle := styles.Muted
			switch c.State {
			case "running":
				stateStyle = styles.Success
			case "restarting":
				stateStyle = styles.Warning
			}
			cName := clip(c.Name, 22)
			b.WriteString(fmt.Sprintf("  %s  %s\n",
				lipgloss.NewStyle().Width(22).Render(cName),
				stateStyle.Render(c.State)))
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

func (m Model) visibleServices() []service.Service {
	if m.filter == "" {
		return m.services
	}
	var out []service.Service
	f := strings.ToLower(m.filter)
	for _, s := range m.services {
		if strings.Contains(strings.ToLower(s.Name), f) {
			out = append(out, s)
		}
	}
	return out
}

func (m Model) selectedService() *service.Service {
	visible := m.visibleServices()
	if len(visible) == 0 || m.cursor >= len(visible) {
		return nil
	}
	svc := visible[m.cursor]
	return &svc
}

func (m Model) selectedName() string {
	svc := m.selectedService()
	if svc == nil {
		return ""
	}
	return svc.Name
}

func (m Model) rootEnv() map[string]string {
	if m.buildEnv == nil {
		return map[string]string{}
	}
	return m.buildEnv("")
}

func (m Model) isTunnelConfigured() bool {
	return m.rootEnv()["CF_TUNNEL_TOKEN"] != ""
}

func (m *Model) busyOp(msg string) {
	m.state = stateBusy
	m.busyMsg = msg
	m.lastMsg = ""
	m.lastErr = ""
}

func (m *Model) uiIdle() {
	if m.state == stateBusy {
		m.state = stateNormal
		m.busyMsg = ""
	}
}

func (m Model) fetchLogsCmd() tea.Cmd {
	svc := m.selectedService()
	if svc == nil || !svc.Installed {
		return nil
	}
	name := svc.Name
	repoRoot := m.repoRoot
	buildEnv := m.buildEnv
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		env := resolveEnv(buildEnv, name)
		_ = r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name),
			env,
			"logs", "--tail", fmt.Sprintf("%d", logTailLines), "--no-color",
		)
		raw := strings.TrimSpace(buf.String())
		lines := strings.Split(raw, "\n")
		var kept []string
		for _, l := range lines {
			if l != "" {
				kept = append(kept, l)
			}
		}
		if len(kept) > logTailLines {
			kept = kept[len(kept)-logTailLines:]
		}
		return logTailMsg{svcName: name, lines: kept}
	}
}

func resolveEnv(fn EnvBuilderFn, name string) map[string]string {
	if fn == nil {
		return nil
	}
	return fn(name)
}

func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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

// ── tea.Cmd functions ─────────────────────────────────────────────────────────

func refreshCmd(repoRoot string, dc *docker.Client, catalogNames []string) tea.Cmd {
	return func() tea.Msg {
		var (
			svcs []service.Service
			err  error
		)
		if dc != nil {
			svcs, err = service.DiscoverAllWithDocker(repoRoot, dc, catalogNames)
		} else {
			svcs, err = service.DiscoverWithCatalog(repoRoot, catalogNames)
		}
		if err != nil {
			return opErrMsg{err: err}
		}
		return refreshedMsg{services: svcs}
	}
}

func coreRefreshCmd(dc *docker.Client) tea.Cmd {
	return func() tea.Msg {
		if dc == nil {
			return coreStatusMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ts := dc.ContainerState(ctx, "tailscale")
		cdy := dc.ContainerState(ctx, "caddy")
		cf := dc.ContainerState(ctx, "cloudflared")
		return coreStatusMsg{ts: ts, caddy: cdy, cloudflared: cf}
	}
}

func coreTickCmd() tea.Cmd {
	return tea.Tick(coreRefreshSec*time.Second, func(t time.Time) tea.Msg {
		return coreTickMsg{}
	})
}

func logTickCmd() tea.Cmd {
	return tea.Tick(logRefreshSec*time.Second, func(t time.Time) tea.Msg {
		return logTickMsg{}
	})
}

func privateEnableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).Enable(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " private route enabled"}
	}
}

func privateDisableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).Disable(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " private route disabled"}
	}
}

func publicEnableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).EnablePublic(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " public route enabled"}
	}
}

func publicDisableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).DisablePublic(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " public route disabled"}
	}
}

func bothEnableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		mgr := caddy.NewWithRunner(repoRoot, r)
		if err := mgr.Enable(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		if err := mgr.EnablePublic(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " private + public enabled"}
	}
}

func bothDisableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).DisableBoth(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " all routes disabled"}
	}
}

func upCmd(repoRoot, name string, buildEnv EnvBuilderFn) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		env := resolveEnv(buildEnv, name)
		if err := r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name), env, "up", "-d",
		); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " started"}
	}
}

func downCmd(repoRoot, name string, buildEnv EnvBuilderFn) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		_ = caddy.NewWithRunner(repoRoot, r).DisableBoth(name)
		env := resolveEnv(buildEnv, name)
		if err := r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name), env, "down",
		); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " stopped"}
	}
}

func restartCmd(repoRoot, name string, buildEnv EnvBuilderFn) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		env := resolveEnv(buildEnv, name)
		if err := r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name), env, "restart",
		); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " restarted"}
	}
}
