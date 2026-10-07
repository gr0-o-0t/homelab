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

// layerChoice is one entry in the enable/disable prompt: the key pressed, the
// layer it selects, and the `homelab enable|disable` flag that does it. The
// private layer has no flag — it is what the bare command does.
type layerChoice struct{ key, layer, flag string }

var layerChoices = []layerChoice{
	{"p", "ts", ""},
	{"c", "cf", "--cf"},
	{"t", "tor", "--tor"},
	{"i", "i2p", "--i2p"},
	{"y", "ygg", "--ygg"},
}

// ── messages ──────────────────────────────────────────────────────────────────

type (
	refreshedMsg struct{ services []service.Service }
	opDoneMsg    struct{ msg string }
	opErrMsg     struct {
		err    error
		output string
	}
	// coreStatusMsg maps a core container name to its state.
	coreStatusMsg map[string]string
	logTailMsg    struct {
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

// EnvBuilderFn returns the docker compose environment map for a service name.
type EnvBuilderFn func(svcName string) map[string]string

// ── model ─────────────────────────────────────────────────────────────────────

// Model is the Bubble Tea model for the full-screen dashboard.
type Model struct {
	// layout
	width, height int
	help          bool

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
	cli          []string // argv prefix that runs the homelab CLI

	// core health header, keyed by container name
	core map[string]string

	// detail pane log tail
	logLines         []string
	logSvcName       string
	containerDetails []docker.ContainerDetail

	// key sequence tracking
	lastKey string // for detecting multi-key sequences (gg)

	// exit signals
	SelectedCoreLogs   bool
	SelectedForLogs    string
	SelectedForNew     bool
	SelectedForInstall string // catalog service name chosen for installation
}

// New constructs the dashboard Model.
// catalogNames lists all names from the embedded service catalog; services not
// yet installed appear in the list as available-to-install stubs.
// layers lists the configured network layers. cli is the argv prefix every
// action is run through (the homelab binary plus its global flags).
func New(repoRoot string, dc *docker.Client, services []service.Service, catalogNames []string, layers []network.NetworkLayer, buildEnv EnvBuilderFn, cli []string) Model {
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
		cli:          cli,
		spin:         sp,
	}
}

// coreDir marks the pinned core row. Its actions run the CLI's no-service
// forms — `homelab up`, `restart`, `update`, `logs` — which act on the core
// stack. Dir is empty for catalog stubs and absolute for real services, so the
// marker cannot collide with either.
const coreDir = "@core"

func isCore(svc *service.Service) bool { return svc != nil && svc.Dir == coreDir }

// coreService is the core stack as a list row, counted from the header's
// container states.
func (m Model) coreService() service.Service {
	s := service.Service{Name: "core", Installed: true, Dir: coreDir}
	for _, c := range m.coreContainers() {
		s.Total++
		if m.core[c] == "running" {
			s.Running++
		}
	}
	return s
}

// coreContainers is caddy plus each configured layer's container.
func (m Model) coreContainers() []string {
	names := []string{"caddy"}
	for _, l := range m.layers {
		names = append(names, l.ContainerName())
	}
	return names
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		refreshCmd(m.repoRoot, m.dc, m.catalogNames),
		coreRefreshCmd(m.dc, m.coreContainers()),
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
		// Clamp cursor if the list shrank.
		if visible := m.visibleServices(); m.cursor >= len(visible) && len(visible) > 0 {
			m.cursor = len(visible) - 1
		}
		cmds = append(cmds, m.fetchLogsCmd())

	case opDoneMsg:
		m.state, m.busyMsg = stateNormal, ""
		m.lastMsg, m.lastErr = msg.msg, ""
		cmds = append(cmds, refreshCmd(m.repoRoot, m.dc, m.catalogNames))

	case opErrMsg:
		m.state, m.busyMsg = stateNormal, ""
		m.lastMsg = ""
		if msg.output != "" {
			m.lastErr = clip(msg.output, 160)
		} else {
			m.lastErr = msg.err.Error()
		}
		// A failed op can still have changed something (a route written
		// before the reload failed), so show the real state.
		cmds = append(cmds, refreshCmd(m.repoRoot, m.dc, m.catalogNames))

	case coreStatusMsg:
		m.core = msg

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
		// The service list refreshes on the same tick: containers change
		// state behind the dashboard's back (crashes, other terminals).
		cmds = append(cmds,
			coreRefreshCmd(m.dc, m.coreContainers()),
			refreshCmd(m.repoRoot, m.dc, m.catalogNames),
			coreTickCmd())

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
	k := msg.String()
	if k == "ctrl+c" {
		return m, append(cmds, tea.Quit)
	}

	switch m.state {

	// ── filter input ──────────────────────────────────────────────────────────
	case stateFilterInput:
		switch k {
		case "enter":
			m.state = stateNormal
		case "esc":
			m.state = stateNormal
			m.filter = ""
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
		cmds = append(cmds, m.fetchLogsCmd())

	// ── layer selection prompts ───────────────────────────────────────────────
	case stateEnablePrompt, stateDisablePrompt:
		svc := m.selectedService()
		if k == "esc" || svc == nil {
			m.state = stateNormal
			break
		}
		verb := "enable"
		if m.state == stateDisablePrompt {
			verb = "disable"
		}
		choices := m.promptChoices(svc)
		var flags []string
		switch {
		case k == "a" && verb == "enable":
			for _, c := range choices {
				if c.flag != "" {
					flags = append(flags, c.flag)
				}
			}
		case k == "a":
			flags = []string{"--all"} // every layer, private included
		default:
			found := false
			for _, c := range choices {
				if c.key == k {
					found = true
					if c.flag != "" {
						flags = []string{c.flag}
					}
				}
			}
			if !found {
				return m, cmds // not an offered choice; keep the prompt open
			}
		}
		args := append([]string{verb, svc.Name}, flags...)
		m.busyOp(fmt.Sprintf("homelab %s…", strings.Join(args, " ")))
		cmds = append(cmds, m.cliCmd(fmt.Sprintf("%s: %sd %s", svc.Name, verb, layerList(verb, flags)), args...), m.spin.Tick)

	// ── normal mode ───────────────────────────────────────────────────────────
	case stateNormal:
		// Any key acknowledges the last result.
		m.lastMsg, m.lastErr = "", ""
		if k != "g" {
			m.lastKey = ""
		}
		svc := m.selectedService()
		installed := svc != nil && svc.Installed
		core := isCore(svc)

		switch k {
		case "q":
			return m, append(cmds, tea.Quit)

		case "?":
			m.help = !m.help

		case "esc":
			m.help = false
			m.filter = ""

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				cmds = append(cmds, m.fetchLogsCmd())
			}

		case "down", "j":
			if visible := m.visibleServices(); m.cursor < len(visible)-1 {
				m.cursor++
				cmds = append(cmds, m.fetchLogsCmd())
			}

		case "g":
			// gg → jump to top
			if m.lastKey == "g" {
				m.cursor = 0
				cmds = append(cmds, m.fetchLogsCmd())
				m.lastKey = ""
			} else {
				m.lastKey = "g"
			}

		case "G", "end":
			if visible := m.visibleServices(); len(visible) > 0 {
				m.cursor = len(visible) - 1
				cmds = append(cmds, m.fetchLogsCmd())
			}

		case "home":
			m.cursor = 0
			cmds = append(cmds, m.fetchLogsCmd())

		case "ctrl+u", "pgup":
			m.cursor -= m.halfPage()
			if m.cursor < 0 {
				m.cursor = 0
			}
			cmds = append(cmds, m.fetchLogsCmd())

		case "ctrl+d", "pgdown":
			if visible := m.visibleServices(); m.cursor+m.halfPage() >= len(visible) {
				m.cursor = max(len(visible)-1, 0)
			} else {
				m.cursor += m.halfPage()
			}
			cmds = append(cmds, m.fetchLogsCmd())

		case "/":
			m.state = stateFilterInput
			m.filter = ""
			m.cursor = 0

		case "R":
			cmds = append(cmds, refreshCmd(m.repoRoot, m.dc, m.catalogNames))

		case "n":
			m.SelectedForNew = true
			return m, append(cmds, tea.Quit)

		// Actions on the selected service. Each runs the CLI command it is
		// named after — the hint text and the command are the same word.
		case "enter":
			switch {
			case core:
				m.SelectedCoreLogs = true
				return m, append(cmds, tea.Quit)
			case installed:
				m.SelectedForLogs = svc.Name
				return m, append(cmds, tea.Quit)
			case svc != nil:
				m.SelectedForInstall = svc.Name
				return m, append(cmds, tea.Quit)
			}

		case "i":
			if svc != nil && !installed {
				m.SelectedForInstall = svc.Name
				return m, append(cmds, tea.Quit)
			}

		case "l":
			if core {
				m.SelectedCoreLogs = true
				return m, append(cmds, tea.Quit)
			}
			if installed {
				m.SelectedForLogs = svc.Name
				return m, append(cmds, tea.Quit)
			}

		case "u", "s", "r", "x", "U":
			if !installed {
				break
			}
			verb := map[string]string{"u": "up", "s": "stop", "r": "restart", "x": "down", "U": "update"}[k]
			done := map[string]string{"up": "started", "stop": "stopped", "restart": "restarted",
				"down": "taken down", "update": "updated"}[verb]
			args := []string{verb, svc.Name}
			if core {
				// Stopping the core takes down every route, including the
				// ones this dashboard's services are reached by: keep that a
				// deliberate shell command.
				if verb == "stop" || verb == "down" {
					m.lastErr = "run `homelab " + verb + "` in a shell to " + verb + " the core stack"
					break
				}
				args = []string{verb}
			}
			m.busyOp("homelab " + strings.Join(args, " ") + "…")
			cmds = append(cmds, m.cliCmd(svc.Name+" "+done, args...), m.spin.Tick)

		case "e", "d":
			if !installed || core {
				break
			}
			m.state = stateEnablePrompt
			if k == "d" {
				m.state = stateDisablePrompt
			}
			if len(m.promptChoices(svc)) == 0 {
				m.state = stateNormal
				m.lastErr = "no network layers to " + map[string]string{"e": "enable", "d": "disable"}[k] + " for " + svc.Name
			}
		}
	}

	return m, cmds
}

// promptChoices lists the layers the current prompt can act on: every
// configured layer when enabling, only the service's active ones when
// disabling.
func (m Model) promptChoices(svc *service.Service) []layerChoice {
	active := map[string]bool{}
	for _, n := range svc.ActiveLayers() {
		active[string(n)] = true
	}
	var out []layerChoice
	for _, c := range layerChoices {
		if _, ok := m.layerByName(c.layer); !ok {
			continue
		}
		if m.state == stateDisablePrompt && !active[c.layer] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// layerList names the layers a CLI call touches, for the status message.
// `enable` always includes the private route; `disable` with flags does not.
func layerList(verb string, flags []string) string {
	switch {
	case len(flags) == 0:
		return "private route"
	case flags[0] == "--all":
		return "all layers"
	}
	names := strings.ReplaceAll(strings.Join(flags, " + "), "--", "")
	if verb == "enable" {
		return "private + " + names
	}
	return names
}

func (m Model) halfPage() int {
	return max((m.height-headerLines-statusbarLines)/2, 1)
}

// ── selection helpers ─────────────────────────────────────────────────────────

// layerByName finds a configured layer by its short name.
func (m Model) layerByName(name string) (network.NetworkLayer, bool) {
	for _, l := range m.layers {
		if l.Name() == name {
			return l, true
		}
	}
	return nil, false
}

func (m Model) visibleServices() []service.Service {
	f := strings.ToLower(m.filter)
	out := make([]service.Service, 0, len(m.services)+1)
	if strings.Contains("core", f) {
		out = append(out, m.coreService())
	}
	if f == "" {
		return append(out, m.services...)
	}
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

func (m *Model) busyOp(msg string) {
	m.state = stateBusy
	m.busyMsg = msg
	m.lastMsg = ""
	m.lastErr = ""
}

func resolveEnv(fn EnvBuilderFn, name string) map[string]string {
	if fn == nil {
		return nil
	}
	return fn(name)
}
