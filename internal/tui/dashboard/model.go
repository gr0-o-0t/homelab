// Package dashboard implements the full-screen homelab dashboard TUI.
//
// Layout: header bar (core and layer status pills) | view tabs | body (a list
// and a detail pane, or a modal: palette, form, confirmation, output, help,
// setup) | footer (context hints, progress, results).
//
// The dashboard keeps no list of its own of what can be done: every action is
// an entry in internal/actions, offered for whatever is selected through
// actions.For, and run as the CLI command it describes. The only hand-written
// table is keys.go's map from a few single keys to action IDs.
package dashboard

import (
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/logs"
	"github.com/groot/homelab/internal/tui/styles"
)

// ── constants ─────────────────────────────────────────────────────────────────

const (
	headerLines       = 2 // status bar + tabs
	statusbarLines    = 1
	logTailLines      = 10
	coreRefreshSec    = 5
	logRefreshSec     = 4
	inspectRefreshSec = 5
)

// ── views and modes ───────────────────────────────────────────────────────────

type view int

const (
	viewServices view = iota
	viewCatalog
	viewNetwork
	viewBackups
	viewHealth
	numViews
)

var viewNames = [numViews]string{"Services", "Catalog", "Network", "Backups", "Health"}
var viewIcons = [numViews]string{"services", "catalog", "network", "backups", "health"}

// mode is what has the keyboard: the list, or one modal over it.
type mode int

const (
	modeNormal  mode = iota
	modeFilter       // typing a list filter
	modePalette      // command palette
	modeForm         // an action's inputs
	modeConfirm      // yes/no before a Confirm action
	modeTyped        // type the token before a TypeName action
	modeOutput       // a command's captured output
	modeHelp         // the keymap
	modeSetup        // vars and secrets of a service (or the root config)
	modeLogs         // the embedded log viewer
)

// ── messages ──────────────────────────────────────────────────────────────────

type (
	refreshedMsg struct {
		services []service.Service
		enabled  map[string]bool // nil: unchanged
	}
	refreshErrMsg struct{ err error }
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
	backupsMsg struct {
		list []backup.Listing
		err  error
	}
	// actionDoneMsg reports a captured (non-interactive, non-stream) run.
	actionDoneMsg struct {
		p   pending
		res result
	}
	// execDoneMsg reports the end of an interactive command the TUI was
	// suspended for.
	execDoneMsg struct {
		label string
		err   error
	}
	choicesMsg struct {
		key     string
		choices []string
		err     error
	}
)

// EnvBuilderFn returns the docker compose environment map for a service name.
type EnvBuilderFn func(svcName string) map[string]string

// Options is what the dashboard needs from its host (cmd).
type Options struct {
	Root     string
	Docker   *docker.Client // nil: no live container state
	Services []service.Service
	Catalog  []string
	// Layers is every registered network layer, the private one first.
	Layers []network.NetworkLayer
	// Enabled reports which extensions config.yaml enables. Called from a
	// tea.Cmd on every refresh, since enabling one changes it; nil means only
	// the private layer.
	Enabled  func() map[string]bool
	BuildEnv EnvBuilderFn
	// CLI is the argv prefix every action runs through: this binary plus its
	// global flags.
	CLI []string
}

// pending is an action on its way to running: inputs collected so far and the
// view whose pane should show the result.
type pending struct {
	action actions.Action
	target actions.Target
	inputs actions.Inputs
	origin view
}

// result is the captured outcome of a command.
type result struct {
	title string
	argv  []string
	out   string
	err   error
}

// streams tracks running log streams so they can be stopped from outside the
// Update loop (signals). Shared by every copy of the Model.
type streams struct {
	mu   sync.Mutex
	stop []func()
}

func (s *streams) add(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop = append(s.stop, f)
}

func (s *streams) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.stop {
		f()
	}
	s.stop = nil
}

// ── model ─────────────────────────────────────────────────────────────────────

// Model is the Bubble Tea model for the full-screen dashboard.
type Model struct {
	opt Options

	width, height int
	view          view
	mode          mode

	cursor [numViews]int
	filter [numViews]string
	marked map[string]bool // multi-selection in the Services view

	services []service.Service
	enabled  map[string]bool
	core     map[string]string // container name → state
	backups  []backup.Listing
	backErr  string

	// Health view: the last result of each check, by action ID.
	health       map[string]result
	healthScroll int

	// detail pane of the selected service
	logLines         []string
	logSvcName       string
	containerDetails []docker.ContainerDetail

	lastKey string // multi-key sequences (gg)

	// progress and results
	running  int
	spin     spinner.Model
	busyMsg  string
	toast    string
	toastErr bool
	lastOut  *result

	// modals
	pal     palette
	frm     form
	confirm confirmState
	out     outputState
	setup   setupForm
	logv    logs.Model
	helpTop int

	streams *streams
}

// New constructs the dashboard Model.
func New(opt Options) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Primary
	return Model{
		opt:      opt,
		services: opt.Services,
		enabled:  map[string]bool{},
		core:     map[string]string{},
		marked:   map[string]bool{},
		health:   map[string]result{},
		spin:     sp,
		streams:  &streams{},
	}
}

// StopStreams kills every log stream the dashboard started. Safe to call from
// another goroutine (a signal handler) while the program runs.
func (m Model) StopStreams() { m.streams.stopAll() }

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.refreshCmd(),
		m.coreRefreshCmd(),
		m.backupsCmd(),
		coreTickCmd(),
		logTickCmd(),
		m.fetchInspectCmd(),
		inspectTickCmd(),
	)
}

// ── network layers ────────────────────────────────────────────────────────────

// offered is the private layer plus every enabled extension: the layers a
// service can be exposed on (actions.Target.Layers).
func (m Model) offered() []string {
	var out []string
	for _, l := range m.opt.Layers {
		if l.Flag() == "" || m.enabled[l.Name()] {
			out = append(out, l.Name())
		}
	}
	return out
}

// offeredLayers is offered as layer values, in registry order.
func (m Model) offeredLayers() []network.NetworkLayer {
	var out []network.NetworkLayer
	for _, l := range m.opt.Layers {
		if l.Flag() == "" || m.enabled[l.Name()] {
			out = append(out, l)
		}
	}
	return out
}

// extensions are the optional layers: the Layer-scope rows of the Network view.
func (m Model) extensions() []network.NetworkLayer {
	var out []network.NetworkLayer
	for _, l := range m.opt.Layers {
		if l.Flag() != "" {
			out = append(out, l)
		}
	}
	return out
}

// coreContainers is caddy plus every layer's container.
func (m Model) coreContainers() []string {
	names := []string{caddyContainer}
	for _, l := range m.opt.Layers {
		names = append(names, l.ContainerName())
	}
	return names
}

// coreHeaderContainers is caddy plus the offered layers' containers: what the
// core stack runs right now.
func (m Model) coreHeaderContainers() []string {
	names := []string{caddyContainer}
	for _, l := range m.offeredLayers() {
		names = append(names, l.ContainerName())
	}
	return names
}

func (m Model) layerRunning(l network.NetworkLayer) bool {
	return m.core[l.ContainerName()] == containerStateRunning
}

// layerIcon is the icon the registry gives exposing a service on the layer.
func layerIcon(name string) string {
	id := "service.enable." + name
	if name == "ts" {
		id = "service.enable"
	}
	if a, ok := actions.ByID(id); ok {
		return a.Icon
	}
	return "link"
}

// ── rows of each view ─────────────────────────────────────────────────────────

// Core container name and the docker state the core header counts as up.
const (
	caddyContainer        = "caddy"
	containerStateRunning = "running"
)

// coreDir marks the pinned core row of the Services view.
const coreDir = "@core"

func isCore(svc *service.Service) bool { return svc != nil && svc.Dir == coreDir }

// coreService is the core stack as a list row, counted from container states.
func (m Model) coreService() service.Service {
	s := service.Service{Name: "core", Installed: true, Dir: coreDir}
	for _, c := range m.coreHeaderContainers() {
		s.Total++
		if m.core[c] == containerStateRunning {
			s.Running++
		}
	}
	return s
}

func matches(name, filter string) bool {
	return filter == "" || strings.Contains(strings.ToLower(name), strings.ToLower(filter))
}

// visibleServices is the Services view: the core row, then installed services.
func (m Model) visibleServices() []service.Service {
	f := m.filter[viewServices]
	out := make([]service.Service, 0, len(m.services)+1)
	if matches("core", f) {
		out = append(out, m.coreService())
	}
	for i := range m.services {
		s := &m.services[i]
		if s.Installed && matches(s.Name, f) {
			out = append(out, *s)
		}
	}
	return out
}

// visibleCatalog is the Catalog view: services not installed yet.
func (m Model) visibleCatalog() []service.Service {
	var out []service.Service
	for i := range m.services {
		s := &m.services[i]
		if !s.Installed && matches(s.Name, m.filter[viewCatalog]) {
			out = append(out, *s)
		}
	}
	return out
}

// healthRow is one check of the Health view.
type healthRow struct {
	a actions.Action
	t actions.Target
}

// healthRows are the stack-wide and core inspections that print a report:
// derived from the registry, not listed here.
func (m Model) healthRows() []healthRow {
	var out []healthRow
	for _, t := range []actions.Target{m.globalTarget(), m.coreTarget()} {
		targetActs := actions.For(t)
		for i := range targetActs {
			a := &targetActs[i]
			if a.Group == actions.GroupInspect && !a.Stream && !a.Interactive {
				out = append(out, healthRow{*a, t})
			}
		}
	}
	return out
}

func (m Model) rowCount(v view) int {
	switch v {
	case viewServices:
		return len(m.visibleServices())
	case viewCatalog:
		return len(m.visibleCatalog())
	case viewNetwork:
		return 1 + len(m.extensions())
	case viewBackups:
		return len(m.backups)
	case viewHealth:
		return len(m.healthRows())
	}
	return 0
}

func (m Model) selectedService() *service.Service {
	var list []service.Service
	switch m.view {
	case viewServices:
		list = m.visibleServices()
	case viewCatalog:
		list = m.visibleCatalog()
	default:
		return nil
	}
	c := m.cursor[m.view]
	if c < 0 || c >= len(list) {
		return nil
	}
	svc := list[c]
	return &svc
}

func (m Model) selectedName() string {
	if svc := m.selectedService(); svc != nil {
		return svc.Name
	}
	return ""
}

// selectedLayer is the Network view's extension row, nil on the core row.
func (m Model) selectedLayer() network.NetworkLayer {
	if m.view != viewNetwork || m.cursor[viewNetwork] == 0 {
		return nil
	}
	ext := m.extensions()
	if i := m.cursor[viewNetwork] - 1; i < len(ext) {
		return ext[i]
	}
	return nil
}

func (m Model) selectedBackup() *backup.Listing {
	if m.view != viewBackups {
		return nil
	}
	if c := m.cursor[viewBackups]; c >= 0 && c < len(m.backups) {
		return &m.backups[c]
	}
	return nil
}

func (m Model) selectedHealth() *healthRow {
	if m.view != viewHealth {
		return nil
	}
	rows := m.healthRows()
	if c := m.cursor[viewHealth]; c >= 0 && c < len(rows) {
		return &rows[c]
	}
	return nil
}

// ── targets ───────────────────────────────────────────────────────────────────

func (m Model) globalTarget() actions.Target { return actions.GlobalTarget(m.offered()) }

func (m Model) coreTarget() actions.Target {
	t := actions.CoreTarget(m.offered())
	c := m.coreService()
	t.Running, t.Total = c.Running, c.Total
	return t
}

func (m Model) layerTarget(l network.NetworkLayer) actions.Target {
	return actions.LayerTarget(l.Name(), m.enabled[l.Name()], m.layerRunning(l))
}

// markedNames are the multi-selected services, in list order.
func (m Model) markedNames() []string {
	var out []string
	for i := range m.services {
		s := &m.services[i]
		if s.Installed && m.marked[s.Name] {
			out = append(out, s.Name)
		}
	}
	return out
}

// multiTarget is the multi-selection as one Service target: Names lists the
// services, counts are summed, and only layers every one is exposed on count.
func (m Model) multiTarget() actions.Target {
	names := m.markedNames()
	t := actions.Target{Scope: actions.Service, Names: names, Layers: m.offered()}
	first := true
	for i := range m.services {
		s := &m.services[i]
		if !s.Installed || !m.marked[s.Name] {
			continue
		}
		t.Running += s.Running
		t.Total += s.Total
		if first {
			t.Exposed = slices.Clone(s.Layers)
			first = false
			continue
		}
		t.Exposed = slices.DeleteFunc(t.Exposed, func(l string) bool { return !slices.Contains(s.Layers, l) })
	}
	if len(names) == 1 {
		t.Name = names[0]
	}
	return t
}

// target is what the selection in the current view acts on; false when
// nothing is selected. Views without their own targets act on the stack.
func (m Model) target() (actions.Target, bool) {
	switch m.view {
	case viewServices:
		if len(m.markedNames()) > 0 {
			return m.multiTarget(), true
		}
		svc := m.selectedService()
		if svc == nil {
			return actions.Target{}, false
		}
		if isCore(svc) {
			return m.coreTarget(), true
		}
		return actions.ServiceTarget(*svc, m.offered()), true
	case viewCatalog:
		svc := m.selectedService()
		if svc == nil {
			return actions.Target{}, false
		}
		return actions.ServiceTarget(*svc, m.offered()), true
	case viewNetwork:
		if l := m.selectedLayer(); l != nil {
			return m.layerTarget(l), true
		}
		return m.coreTarget(), true
	}
	return m.globalTarget(), true
}

// prefill are input values the selection implies: the selected backup.
func (m Model) prefill() actions.Inputs {
	if b := m.selectedBackup(); b != nil {
		return actions.Inputs{"backup": b.Dir}
	}
	return nil
}

// targetTitle names a target for headings and confirmations.
func targetTitle(t actions.Target) string {
	switch {
	case len(t.Names) > 1:
		return strings.Join(t.Names, ", ")
	case t.Scope == actions.Global:
		return "stack"
	case t.Scope == actions.Core:
		return "core"
	case t.Name != "":
		return t.Name
	case len(t.Names) == 1:
		return t.Names[0]
	}
	return t.Scope.String()
}

// ── small helpers ─────────────────────────────────────────────────────────────

func (m Model) bodyHeight() int { return max(m.height-headerLines-statusbarLines, 1) }

func (m Model) halfPage() int { return max(m.bodyHeight()/2, 1) }

func (m Model) clampCursors() Model {
	for v := view(0); v < numViews; v++ {
		n := m.rowCount(v)
		if m.cursor[v] >= n {
			m.cursor[v] = max(n-1, 0)
		}
		if m.cursor[v] < 0 {
			m.cursor[v] = 0
		}
	}
	return m
}

func (m Model) rootEnv() map[string]string {
	if m.opt.BuildEnv == nil {
		return map[string]string{}
	}
	return m.opt.BuildEnv("")
}

func resolveEnv(fn EnvBuilderFn, name string) map[string]string {
	if fn == nil {
		return nil
	}
	return fn(name)
}

// withMark returns a copy of the marks with name toggled; Models are values,
// so the map is never mutated in place.
func (m Model) withMark(name string) map[string]bool {
	out := maps.Clone(m.marked)
	if out == nil {
		out = map[string]bool{}
	}
	if out[name] {
		delete(out, name)
	} else {
		out[name] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
