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

// shared is the state background goroutines write.
type shared struct {
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.status, a.failed = err.Error(), true
		return
	}
	a.services = svcs
	if a.selected == "" && len(svcs) > 0 {
		a.selected = svcs[0].Name
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
		argv := append(append([]string{}, a.opt.CLI...), "logs", "-n", "200", name)
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
	for _, verb := range []string{"up", "stop", "restart", "down"} {
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
		if layer != "ts" {
			flag = []string{"--" + layer}
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
