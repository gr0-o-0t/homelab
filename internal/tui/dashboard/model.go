// Package dashboard implements the full-screen homelab dashboard TUI.
// Layout: header bar | left service list | right detail pane | status bar.
package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// ── constants ─────────────────────────────────────────────────────────────────

const (
	leftPaneWidth     = 40 // full left column width (including separator)
	listInnerWidth    = 38 // usable characters inside the left pane
	headerLines       = 1
	statusbarLines    = 1
	logTailLines      = 10
	coreRefreshSec    = 5
	logRefreshSec     = 4
	inspectRefreshSec = 5

	// Name column widths inside the list. Derived from listInnerWidth.
	//   installed item: cursor(2) + dot(1) + space(1) + name + space(1) + badge(7) = 12 + name
	//   catalog  item:  cursor(2) + plus(1) + space(1) + name                       = 4 + name
	installedNameW = listInnerWidth - 12 // 26
	catalogNameW   = listInnerWidth - 4  // 34
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
	refreshedMsg struct{ services []service.Service }
	opDoneMsg    struct{ msg string }
	opErrMsg     struct {
		err    error
		output string
	}
	coreStatusMsg struct {
		ts, caddy, cloudflared string
		tor, i2p, yggdrasil    string
	}
	logTailMsg struct {
		svcName string
		lines   []string
	}
	coreTickMsg        struct{}
	logTickMsg         struct{}
	inspectTickMsg     struct{}
	containerDetailMsg struct {
		svcName string
		details []docker.ContainerDetail
	}
)

// ── core status ───────────────────────────────────────────────────────────────

type coreStatus struct {
	tailscale   string
	caddy       string
	cloudflared string
	tor         string
	i2p         string
	yggdrasil   string
}

// layerByName finds a registered layer by its short name.

// layerByName finds a registered layer by its short name.
func (m Model) layerByName(name string) (network.NetworkLayer, bool) {
	for _, l := range m.layers {
		if l.Name() == name {
			return l, true
		}
	}
	return nil, false
}

func (cs coreStatus) get(containerName string) string {
	switch containerName {
	case "tailscale":
		return cs.tailscale
	case "caddy":
		return cs.caddy
	case "cloudflared":
		return cs.cloudflared
	case "tor":
		return cs.tor
	case "i2p":
		return cs.i2p
	case "yggdrasil":
		return cs.yggdrasil
	default:
		return ""
	}
}

// ── EnvBuilderFn ──────────────────────────────────────────────────────────────

// EnvBuilderFn returns the docker compose environment map for a service name.

// EnvBuilderFn returns the docker compose environment map for a service name.
type EnvBuilderFn func(svcName string) map[string]string

// ── model ─────────────────────────────────────────────────────────────────────

// Model is the Bubble Tea model for the full-screen dashboard.

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
	layers       []network.NetworkLayer
	cursor       int
	filter       string
	buildEnv     EnvBuilderFn

	// core health header
	core coreStatus

	// detail pane log tail
	logLines         []string
	logSvcName       string
	containerDetails []docker.ContainerDetail

	// key sequence tracking
	lastKey string // for detecting multi-key sequences (gg)

	// exit signals
	SelectedForLogs    string
	SelectedForNew     bool
	SelectedForInstall string // catalog service name chosen for installation
}

// New constructs the dashboard Model.
// catalogNames lists all names from the embedded service catalog; services not
// yet installed appear in the list as available-to-install stubs.
// layers lists registered network layers for header status pills.

// New constructs the dashboard Model.
// catalogNames lists all names from the embedded service catalog; services not
// yet installed appear in the list as available-to-install stubs.
// layers lists registered network layers for header status pills.
func New(repoRoot string, dc *docker.Client, services []service.Service, catalogNames []string, layers []network.NetworkLayer, buildEnv EnvBuilderFn) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Primary

	return Model{
		repoRoot:     repoRoot,
		dc:           dc,
		services:     services,
		catalogNames: catalogNames,
		layers:       layers,
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
		inspectCmd(m.repoRoot, m.dc, m.selectedName()),
		inspectTickCmd(),
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
			tor:         msg.tor,
			i2p:         msg.i2p,
			yggdrasil:   msg.yggdrasil,
		}

	case logTailMsg:
		if msg.svcName == m.selectedName() {
			m.logLines = msg.lines
			m.logSvcName = msg.svcName
		}

	case containerDetailMsg:
		if msg.svcName == m.selectedName() {
			m.containerDetails = msg.details
		}

	case coreTickMsg:
		cmds = append(cmds, coreRefreshCmd(m.dc), coreTickCmd())

	case logTickMsg:
		cmds = append(cmds, m.fetchLogsCmd(), logTickCmd())

	case inspectTickMsg:
		cmds = append(cmds, m.fetchInspectCmd(), inspectTickCmd())

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
		// Reset key sequence tracker on any non-g key.
		if msg.String() != "g" && msg.String() != "ctrl+u" && msg.String() != "ctrl+d" {
			m.lastKey = ""
		}

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

		case "g":
			// gg → jump to top
			if m.focused == paneList && m.lastKey == "g" {
				m.cursor = 0
				cmds = append(cmds, m.fetchLogsCmd())
				m.lastKey = ""
			} else {
				m.lastKey = "g"
			}

		case "G":
			// G → jump to bottom
			if m.focused == paneList {
				if visible := m.visibleServices(); len(visible) > 0 {
					m.cursor = len(visible) - 1
					cmds = append(cmds, m.fetchLogsCmd())
				}
			}

		case "ctrl+u":
			// Ctrl+u → half page up
			if m.focused == paneList {
				scrollBy := (m.height - headerLines - statusbarLines) / 2
				if scrollBy < 1 {
					scrollBy = 1
				}
				m.cursor -= scrollBy
				if m.cursor < 0 {
					m.cursor = 0
				}
				cmds = append(cmds, m.fetchLogsCmd())
			}

		case "ctrl+d":
			// Ctrl+d → half page down
			if m.focused == paneList {
				scrollBy := (m.height - headerLines - statusbarLines) / 2
				if scrollBy < 1 {
					scrollBy = 1
				}
				if visible := m.visibleServices(); m.cursor+scrollBy >= len(visible) {
					m.cursor = len(visible) - 1
				} else {
					m.cursor += scrollBy
				}
				cmds = append(cmds, m.fetchLogsCmd())
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

func resolveEnv(fn EnvBuilderFn, name string) map[string]string {
	if fn == nil {
		return nil
	}
	return fn(name)
}
