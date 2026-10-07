//go:build gui

// Package gui is the experimental desktop front end, `homelab --gui`, built on
// Dear ImGui via giu. It is compiled only with `-tags gui` (see `make gui`):
// giu needs cgo and the OpenGL/X11 development headers, which the default,
// pure-Go build must not.
//
// Like the terminal dashboard, it has no logic of its own: each button runs
// the homelab CLI command it is named after, so the GUI cannot disagree with
// the CLI about what an action does.
package gui

import (
	"image/color"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	g "github.com/AllenDang/giu"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/service"
)

const refreshEvery = 5 * time.Second

var (
	colRunning = color.RGBA{0x9E, 0xCE, 0x6A, 0xFF}
	colPartial = color.RGBA{0xE0, 0xAF, 0x68, 0xFF}
	colMuted   = color.RGBA{0x73, 0x7A, 0xA2, 0xFF}
	colErr     = color.RGBA{0xF7, 0x76, 0x8E, 0xFF}
)

type app struct {
	opt   Options
	split float32

	// UI-thread only: the frame is built from v, a copy of the shared state
	// taken under mu, because giu runs widget callbacks while the frame is
	// being built — callbacks that themselves take mu.
	filter string
	v      shared

	mu sync.Mutex
	shared
}

// coreName is the pinned core row. Its buttons run the CLI's no-service forms.
const coreName = "core"

// shared is the state background goroutines write.
type shared struct {
	core     []ContainerState
	services []service.Service
	selected string
	busy     string // label of the running action; one at a time
	status   string
	failed   bool
	logs     string
	logsFor  string
}

// Run opens the window and blocks until it is closed.
func Run(opt Options) error {
	a := &app{opt: opt, split: 360}
	a.refresh()
	go func() {
		for range time.Tick(refreshEvery) {
			a.refresh()
			g.Update()
		}
	}()
	g.NewMasterWindow("homelab", 1180, 720, 0).Run(a.loop)
	return nil
}

func (a *app) refresh() {
	svcs, err := a.opt.Discover()
	var core []ContainerState
	if a.opt.Core != nil {
		core = a.opt.Core()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.core = core
	if err != nil {
		a.status, a.failed = err.Error(), true
		return
	}
	a.services = svcs
	if a.selected == "" {
		a.selected = coreName
	}
}

// do runs `homelab <args>` in the background and reports its result in the
// status line. The CLI's last output line is its error message.
func (a *app) do(label string, args ...string) {
	a.mu.Lock()
	if a.busy != "" {
		a.mu.Unlock()
		return
	}
	a.busy, a.status, a.failed = label, "", false
	a.mu.Unlock()

	go func() {
		argv := append(append([]string{}, a.opt.CLI...), args...)
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		a.mu.Lock()
		a.busy = ""
		if err != nil {
			a.status, a.failed = lastLine(string(out)), true
		} else {
			a.status, a.failed = label+" — done", false
		}
		a.mu.Unlock()
		a.refresh()
		g.Update()
	}()
}

func (a *app) selectService(name string) {
	a.mu.Lock()
	a.selected, a.logsFor, a.logs = name, "", ""
	a.mu.Unlock()
}

// loadLogs fetches the last lines of a service's logs, like `homelab logs -n`.
func (a *app) loadLogs(name string) {
	a.mu.Lock()
	a.logsFor, a.logs = name, "loading…"
	a.mu.Unlock()
	go func() {
		args := []string{"logs", "-n", "200"}
		if name != coreName {
			args = append(args, name)
		}
		argv := append(append([]string{}, a.opt.CLI...), args...)
		out, _ := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		a.mu.Lock()
		if a.logsFor == name {
			a.logs = string(out)
		}
		a.mu.Unlock()
		g.Update()
	}()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// ── layout ────────────────────────────────────────────────────────────────────

func (a *app) loop() {
	a.mu.Lock()
	a.v = a.shared
	a.mu.Unlock()

	g.SingleWindow().Layout(
		a.statusLine(),
		g.Separator(),
		g.SplitLayout(g.DirectionVertical, &a.split, a.listPane(), a.detailPane()),
	)
}

func (a *app) statusLine() g.Widget {
	var running, installed int
	for _, s := range a.v.services {
		if s.Installed {
			installed++
			if s.Running > 0 {
				running++
			}
		}
	}
	summary := g.Label(strconv.Itoa(running) + " running · " + strconv.Itoa(installed) + " installed")
	switch {
	case a.v.busy != "":
		return g.Row(summary, g.Style().SetColor(g.StyleColorText, colPartial).To(g.Label("  "+a.v.busy+"…")))
	case a.v.failed:
		return g.Row(summary, g.Style().SetColor(g.StyleColorText, colErr).To(g.Label("  "+a.v.status)))
	default:
		return g.Row(summary, g.Style().SetColor(g.StyleColorText, colMuted).To(g.Label("  "+a.v.status)))
	}
}

func (a *app) listPane() g.Widget {
	rows := []*g.TableRowWidget{}
	f := strings.ToLower(a.filter)
	if strings.Contains(coreName, f) {
		running := 0
		for _, c := range a.v.core {
			if c.State == "running" {
				running++
			}
		}
		col := colRunning
		if running < len(a.v.core) {
			col = colPartial
		}
		rows = append(rows, g.TableRow(
			g.Selectable(coreName).Selected(a.v.selected == coreName).
				Flags(g.SelectableFlagsSpanAllColumns).OnClick(func() { a.selectService(coreName) }),
			g.Style().SetColor(g.StyleColorText, col).To(g.Label(strconv.Itoa(running)+"/"+strconv.Itoa(len(a.v.core))+" running")),
			g.Label("core stack"),
		))
	}
	for _, s := range a.v.services {
		if f != "" && !strings.Contains(strings.ToLower(s.Name), f) {
			continue
		}
		name := s.Name
		state, col := "available", color.Color(colMuted)
		if s.Installed {
			state, col = stateText(s)
		}
		layers := make([]string, 0, 5)
		for _, l := range s.ActiveLayers() {
			layers = append(layers, string(l))
		}
		rows = append(rows, g.TableRow(
			g.Selectable(name).Selected(name == a.v.selected).
				Flags(g.SelectableFlagsSpanAllColumns).
				OnClick(func() { a.selectService(name) }),
			g.Style().SetColor(g.StyleColorText, col).To(g.Label(state)),
			g.Label(strings.Join(layers, " ")),
		))
	}
	return g.Layout{
		g.InputText(&a.filter).Hint("filter services").Size(-1),
		g.Table().Flags(g.TableFlagsRowBg|g.TableFlagsScrollY|g.TableFlagsBordersInnerV).
			Freeze(0, 1).
			Columns(g.TableColumn("Service"), g.TableColumn("State"), g.TableColumn("Exposed on")).
			Rows(rows...),
	}
}

func stateText(s service.Service) (string, color.Color) {
	switch {
	case s.Total > 0 && s.Running == s.Total:
		return "running", colRunning
	case s.Running > 0:
		return strconv.Itoa(s.Running) + "/" + strconv.Itoa(s.Total) + " running", colPartial
	default:
		return "stopped", colMuted
	}
}

func (a *app) detailPane() g.Widget {
	if a.v.selected == coreName {
		return a.coreDetail()
	}
	var svc *service.Service
	for i := range a.v.services {
		if a.v.services[i].Name == a.v.selected {
			svc = &a.v.services[i]
		}
	}
	if svc == nil {
		return g.Label("Select a service.")
	}
	if !svc.Installed {
		name := svc.Name
		return g.Layout{
			g.Label(name + " — not installed"),
			g.Separator(),
			g.Button("Install").Disabled(a.v.busy != "").OnClick(func() { a.do("homelab add "+name, "add", name) }),
			g.Style().SetColor(g.StyleColorText, colMuted).To(
				g.Label("Then configure it in a terminal: homelab setup " + name).Wrapped(true)),
		}
	}

	name := svc.Name
	state, col := stateText(*svc)
	var buttons []g.Widget
	for _, verb := range []string{"up", "stop", "restart", "down", "update"} {
		buttons = append(buttons, g.Button(verb).Disabled(a.v.busy != "").
			OnClick(func() { a.do("homelab "+verb+" "+name, verb, name) }))
	}
	lifecycle := g.Row(buttons...)

	if a.v.logsFor != name {
		a.loadLogs(name)
	}

	return g.Layout{
		g.Row(g.Label(name), g.Style().SetColor(g.StyleColorText, col).To(g.Label(state))),
		g.Separator(),
		lifecycle,
		g.Spacing(),
		g.Label("Network layers"),
		a.layerToggles(svc),
		g.Spacing(),
		a.containers(svc),
		g.Spacing(),
		g.Row(g.Label("Logs"), g.Button("Reload").OnClick(func() { a.loadLogs(name) })),
		g.InputTextMultiline(&a.v.logs).Flags(g.InputTextFlagsReadOnly).Size(-1, -1),
	}
}

// coreDetail shows the core containers and the core actions. Stop and down
// are left to a shell: the core serves every route, this GUI's included.
func (a *app) coreDetail() g.Widget {
	var buttons []g.Widget
	for _, verb := range []string{"up", "restart", "update"} {
		buttons = append(buttons, g.Button(verb).Disabled(a.v.busy != "").
			OnClick(func() { a.do("homelab "+verb, verb) }))
	}
	rows := make([]*g.TableRowWidget, 0, len(a.v.core))
	for _, c := range a.v.core {
		state, col := c.State, color.Color(colRunning)
		if state != "running" {
			col = colPartial
		}
		if state == "" {
			state, col = "not running", colMuted
		}
		rows = append(rows, g.TableRow(g.Label(c.Name), g.Style().SetColor(g.StyleColorText, col).To(g.Label(state))))
	}
	if a.v.logsFor != coreName {
		a.loadLogs(coreName)
	}
	return g.Layout{
		g.Label("core stack"),
		g.Separator(),
		g.Row(buttons...),
		g.Style().SetColor(g.StyleColorText, colMuted).To(
			g.Label("update refreshes the core files, pulls and rebuilds. Stop/down the core from a shell.").Wrapped(true)),
		g.Spacing(),
		g.Table().Size(-1, float32(len(rows)+1)*24+4).
			Columns(g.TableColumn("Container"), g.TableColumn("State")).Rows(rows...),
		g.Spacing(),
		g.Row(g.Label("Logs"), g.Button("Reload").OnClick(func() { a.loadLogs(coreName) })),
		g.InputTextMultiline(&a.v.logs).Flags(g.InputTextFlagsReadOnly).Size(-1, -1),
	}
}

// layerToggles shows one checkbox per configured layer, ticked when the
// service is exposed on it, plus the addresses that layer resolves. Ticking
// runs `homelab enable <svc> [--flag]`; unticking runs `homelab disable`.
func (a *app) layerToggles(svc *service.Service) g.Widget {
	active := map[string]bool{}
	for _, l := range svc.ActiveLayers() {
		active[string(l)] = true
	}
	env := a.opt.Env("")
	name := svc.Name
	var out g.Layout
	for _, l := range a.opt.Layers {
		layer := l.Name()
		flag := []string{}
		if l.Flag() != "" {
			flag = []string{"--" + l.Flag()}
		}
		on := active[layer]
		label := layer + " — " + l.Label()
		out = append(out, g.Checkbox(label, &on).OnChange(func() {
			verb := "disable"
			if on {
				verb = "enable"
			}
			a.do("homelab "+verb+" "+name+" "+strings.Join(flag, " "), append([]string{verb, name}, flag...)...)
		}))
		if active[layer] {
			for _, addr := range l.ServiceAddresses(name, env) {
				text := addr.URL
				if addr.Note != "" {
					text = strings.TrimSpace(text + " (" + addr.Note + ")")
				}
				out = append(out, g.Style().SetColor(g.StyleColorText, colMuted).To(g.Label("    "+text)))
			}
		}
	}
	return out
}

func (a *app) containers(svc *service.Service) g.Widget {
	if len(svc.Containers) == 0 {
		return g.Label("No containers — press up to create them.")
	}
	cs := append([]docker.ContainerDetail{}, svc.Containers...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	rows := make([]*g.TableRowWidget, 0, len(cs))
	for _, c := range cs {
		col := color.Color(colMuted)
		if c.State == "running" {
			col = colRunning
		}
		rows = append(rows, g.TableRow(g.Label(c.Name), g.Style().SetColor(g.StyleColorText, col).To(g.Label(c.State))))
	}
	return g.Table().Size(-1, float32(len(rows)+1)*24+4).
		Columns(g.TableColumn("Container"), g.TableColumn("State")).Rows(rows...)
}
