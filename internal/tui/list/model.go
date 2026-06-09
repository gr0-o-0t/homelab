// Package list implements the fullscreen interactive service browser.
package list

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// ── list.Item adapter ────────────────────────────────────────────────────────

type svcItem struct{ svc service.Service }

func (i svcItem) Title() string       { return i.svc.Name }
func (i svcItem) Description() string { return "" }
func (i svcItem) FilterValue() string { return i.svc.Name }

// ── item delegate ─────────────────────────────────────────────────────────────

type itemDelegate struct{}

func (d itemDelegate) Height() int                             { return 1 }
func (d itemDelegate) Spacing() int                            { return 0 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d itemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	si, ok := item.(svcItem)
	if !ok {
		return
	}
	svc := si.svc
	running := svc.Running > 0
	selected := index == m.Index()

	// Cursor
	cursor := "  "
	if selected {
		cursor = styles.Primary.Render("▶ ")
	}

	// Name — bold when selected
	nameStyle := lipgloss.NewStyle().Width(18)
	if selected {
		nameStyle = nameStyle.Bold(true)
	}
	name := nameStyle.Render(svc.Name)

	// Exposure badge
	var expose string
	if svc.Enabled {
		expose = styles.Success.Render(lipgloss.NewStyle().Width(9).Render("exposed"))
	} else {
		expose = styles.Muted.Render(lipgloss.NewStyle().Width(9).Render("hidden"))
	}

	// Container count
	var ctrCol string
	switch {
	case svc.Total == 0:
		ctrCol = styles.Muted.Render("stopped")
	case svc.Running == svc.Total:
		ctrCol = styles.Success.Render(fmt.Sprintf("%d running", svc.Running))
	default:
		ctrCol = styles.Warning.Render(fmt.Sprintf("%d/%d running", svc.Running, svc.Total))
	}

	_, _ = fmt.Fprintf(w, " %s%s %s  %s  %s",
		cursor, styles.Dot(running, svc.Enabled), name, expose, ctrCol)
}

// ── messages ──────────────────────────────────────────────────────────────────

type refreshedMsg struct{ services []service.Service }
type opDoneMsg struct{ msg string }
type opErrMsg struct {
	err    error
	output string
}

// ── model ─────────────────────────────────────────────────────────────────────

type uiState int

const (
	stateIdle uiState = iota
	stateBusy
)

// EnvBuilderFn is a callback that returns the docker compose environment map
// for a given service name. The list model uses it for up/down/restart ops.
type EnvBuilderFn func(svcName string) map[string]string

// Model is the Bubble Tea model for the fullscreen service browser.
//
// When the user presses 'l', SelectedForLogs is set and the program exits so
// the caller can launch the log viewer for that service.
type Model struct {
	list     list.Model
	spin     spinner.Model
	services []service.Service

	repoRoot string
	dc       *docker.Client
	buildEnv EnvBuilderFn

	uiState uiState
	busyMsg string
	lastMsg string
	lastErr string

	// SelectedForLogs is set (non-empty) when the user pressed 'l'.
	// The caller should launch a log viewer for this service name.
	SelectedForLogs string

	// SelectedForNew is set when the user pressed 'n' to scaffold a new service.
	SelectedForNew bool

	width  int
	height int
}

// New constructs the model. Pass w=0, h=0 — the first tea.WindowSizeMsg fills
// them in. buildEnv is called for each service lifecycle operation to obtain
// the full docker compose environment map; pass nil to fall back to .env files.
func New(repoRoot string, dc *docker.Client, services []service.Service, w, h int, buildEnv EnvBuilderFn) Model {
	items := toItems(services)

	l := list.New(items, itemDelegate{}, w, listHeight(h))
	l.SetShowTitle(false)
	l.SetShowHelp(false)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Primary

	return Model{
		list:     l,
		spin:     sp,
		services: services,
		repoRoot: repoRoot,
		dc:       dc,
		buildEnv: buildEnv,
		width:    w,
		height:   h,
	}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	// ── window resize ────────────────────────────────────────────────────────
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.SetSize(msg.Width, listHeight(msg.Height))

	// ── keyboard ─────────────────────────────────────────────────────────────
	case tea.KeyMsg:
		if m.uiState == stateBusy {
			return m, nil // ignore keys while an op is running
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "e":
			if svc := m.selected(); svc != nil {
				m.uiState = stateBusy
				m.busyMsg = fmt.Sprintf("Enabling %s…", svc.Name)
				m.lastErr, m.lastMsg = "", ""
				return m, tea.Batch(enableCmd(m.repoRoot, svc.Name), m.spin.Tick)
			}

		case "d":
			if svc := m.selected(); svc != nil {
				m.uiState = stateBusy
				m.busyMsg = fmt.Sprintf("Disabling %s…", svc.Name)
				m.lastErr, m.lastMsg = "", ""
				return m, tea.Batch(disableCmd(m.repoRoot, svc.Name), m.spin.Tick)
			}

		case "u":
			if svc := m.selected(); svc != nil {
				m.uiState = stateBusy
				m.busyMsg = fmt.Sprintf("Starting %s…", svc.Name)
				m.lastErr, m.lastMsg = "", ""
				return m, tea.Batch(upCmd(m.repoRoot, svc.Name, m.buildEnv), m.spin.Tick)
			}

		case "x":
			if svc := m.selected(); svc != nil {
				m.uiState = stateBusy
				m.busyMsg = fmt.Sprintf("Stopping %s…", svc.Name)
				m.lastErr, m.lastMsg = "", ""
				return m, tea.Batch(downCmd(m.repoRoot, svc.Name, m.buildEnv), m.spin.Tick)
			}

		case "r":
			if svc := m.selected(); svc != nil {
				m.uiState = stateBusy
				m.busyMsg = fmt.Sprintf("Restarting %s…", svc.Name)
				m.lastErr, m.lastMsg = "", ""
				return m, tea.Batch(restartCmd(m.repoRoot, svc.Name, m.buildEnv), m.spin.Tick)
			}

		case "l":
			if svc := m.selected(); svc != nil {
				m.SelectedForLogs = svc.Name
				return m, tea.Quit
			}

		case "n":
			m.SelectedForNew = true
			return m, tea.Quit

		case "R":
			m.uiState = stateBusy
			m.busyMsg = "Refreshing…"
			m.lastErr, m.lastMsg = "", ""
			return m, tea.Batch(refreshCmd(m.repoRoot, m.dc), m.spin.Tick)

		default:
			var lCmd tea.Cmd
			m.list, lCmd = m.list.Update(msg)
			cmds = append(cmds, lCmd)
		}

	// ── async results ─────────────────────────────────────────────────────────
	case refreshedMsg:
		m.services = msg.services
		m.list.SetItems(toItems(msg.services))
		m.uiState = stateIdle
		m.busyMsg = ""

	case opDoneMsg:
		m.lastMsg = msg.msg
		m.lastErr = ""
		return m, refreshCmd(m.repoRoot, m.dc) // auto-refresh after every op

	case opErrMsg:
		m.uiState = stateIdle
		m.busyMsg = ""
		m.lastMsg = ""
		if msg.output != "" {
			m.lastErr = strings.TrimSpace(msg.output)
		} else {
			m.lastErr = msg.err.Error()
		}

	// ── spinner tick ──────────────────────────────────────────────────────────
	case spinner.TickMsg:
		if m.uiState == stateBusy {
			var sCmd tea.Cmd
			m.spin, sCmd = m.spin.Update(msg)
			cmds = append(cmds, sCmd)
		}

	default:
		var lCmd tea.Cmd
		m.list, lCmd = m.list.Update(msg)
		cmds = append(cmds, lCmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}

	var b strings.Builder

	// ── header ────────────────────────────────────────────────────────────────
	exposed := 0
	for _, s := range m.services {
		if s.Enabled {
			exposed++
		}
	}

	var statusPart string
	switch {
	case m.uiState == stateBusy:
		statusPart = styles.Primary.Render(m.spin.View() + " " + m.busyMsg)
	case m.lastErr != "":
		statusPart = styles.Err.Render("✗ " + clip(m.lastErr, m.width-30))
	case m.lastMsg != "":
		statusPart = styles.Success.Render("✓ " + m.lastMsg)
	default:
		statusPart = styles.Muted.Render(
			fmt.Sprintf("%d services / %d exposed", len(m.services), exposed))
	}

	title := styles.Header.Render("Homelab")
	b.WriteString("\n  " + title + "  " + statusPart + "\n\n")

	// ── list ──────────────────────────────────────────────────────────────────
	b.WriteString(m.list.View())

	// ── footer ────────────────────────────────────────────────────────────────
	b.WriteString("\n\n")
	b.WriteString(renderFooter())

	return b.String()
}

// ── helpers ───────────────────────────────────────────────────────────────────

func renderFooter() string {
	type hint struct{ k, v string }
	hints := []hint{
		{"e", "enable"}, {"d", "disable"}, {"u", "start"}, {"x", "stop"},
		{"l", "logs"}, {"r", "restart"}, {"n", "new"}, {"R", "refresh"},
		{"/", "filter"}, {"j", "↓"}, {"k", "↑"}, {"gg", "top"}, {"G", "bot"}, {"q", "quit"},
	}
	parts := make([]string, len(hints))
	for i, h := range hints {
		parts[i] = styles.Muted.Render("[") +
			styles.Primary.Render(h.k) +
			styles.Muted.Render("] "+h.v)
	}
	return "  " + strings.Join(parts, styles.Muted.Render("  ")) + "\n"
}

func (m Model) selected() *service.Service {
	item := m.list.SelectedItem()
	if item == nil {
		return nil
	}
	si, ok := item.(svcItem)
	if !ok {
		return nil
	}
	svc := si.svc
	return &svc
}

func toItems(svcs []service.Service) []list.Item {
	items := make([]list.Item, len(svcs))
	for i, s := range svcs {
		items[i] = svcItem{svc: s}
	}
	return items
}

func listHeight(total int) int {
	h := total - 7 // header (3) + footer (2) + padding (2)
	if h < 1 {
		return 1
	}
	return h
}

func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ── tea.Cmd functions (run in goroutines) ─────────────────────────────────────

func refreshCmd(repoRoot string, dc *docker.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = ctx // DiscoverWithDocker manages its own context internally
		var (
			svcs []service.Service
			err  error
		)
		if dc != nil {
			svcs, err = service.DiscoverWithDocker(repoRoot, dc)
		} else {
			svcs, err = service.Discover(repoRoot)
		}
		if err != nil {
			return opErrMsg{err: err}
		}
		return refreshedMsg{services: svcs}
	}
}

func enableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).Enable(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " exposed"}
	}
}

func disableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).Disable(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " hidden"}
	}
}

func upCmd(repoRoot, name string, buildEnv EnvBuilderFn) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		env := resolveEnv(buildEnv, name)
		if err := r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name),
			env,
			"up", "-d",
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
		env := resolveEnv(buildEnv, name)
		if err := r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name),
			env,
			"down",
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
			run.ServiceComposeFile(repoRoot, name),
			env,
			"restart",
		); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " restarted"}
	}
}

// resolveEnv calls fn(name) if fn is non-nil, otherwise returns nil.
func resolveEnv(fn EnvBuilderFn, name string) map[string]string {
	if fn == nil {
		return nil
	}
	return fn(name)
}
