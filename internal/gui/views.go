//go:build gui

package gui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AllenDang/cimgui-go/imgui"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/service"
)

type viewID int

const (
	viewServices viewID = iota
	viewCatalog
	viewNetwork
	viewBackups
	viewHealth
	viewSettings
)

var views = []struct {
	glyph, label string
}{
	{glyphServices, "Services"},
	{glyphCatalog, "Catalog"},
	{glyphNetwork, "Network"},
	{glyphBackups, "Backups"},
	{glyphHealth, "Health"},
	{glyphSettings, "Settings"},
}

// Service detail tabs.
const (
	tabOverview = iota
	tabNetwork
	tabConfig
	tabLogs
	tabActions
)

// uiState is UI-thread-only state: never touched by background goroutines,
// so it needs no lock.
type uiState struct {
	scale float32
	view  viewID

	filter    string
	catFilter string
	selected  string // service name; "" = the stack
	checked   map[string]bool
	tabReq    int // tab to select next frame, -1 = none

	addrFor string

	cfgFor string
	cfgGen int
	cfg    *setupForm

	rootReq bool
	rootGen int
	rootCfg *setupForm

	logsFor     string
	logsJob     int
	logsVisible bool

	consoleOpen bool
	consoleJob  int // 0 = latest
	healthJob   int

	modal       *modalState
	palette     paletteState
	copyCmd     string
	copyOpen    bool
	choicesReq  map[string]bool
	busyWarning string
}

func (u *uiState) init() {
	u.scale = 1
	u.checked = map[string]bool{}
	u.choicesReq = map[string]bool{}
	u.tabReq = -1
}

func (a *app) px(v float32) float32 { return v * a.ui.scale }

// ── frame ─────────────────────────────────────────────────────────────────────

func (a *app) loop() {
	a.snapshot()
	a.ui.scale = imgui.CurrentStyle().FontScaleDpi()
	if a.ui.scale <= 0 {
		a.ui.scale = 1
	}
	a.ui.logsVisible = false
	a.shortcuts()

	vp := imgui.MainViewport()
	imgui.SetNextWindowPos(vp.WorkPos())
	imgui.SetNextWindowSize(vp.WorkSize())
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(0, 0))
	imgui.PushStyleVarVec2(imgui.StyleVarItemSpacing, v2(0, 0))
	imgui.BeginV("##root", nil, imgui.WindowFlagsNoDecoration|imgui.WindowFlagsNoMove|imgui.WindowFlagsNoSavedSettings|
		imgui.WindowFlagsNoBringToFrontOnFocus|imgui.WindowFlagsNoScrollbar|imgui.WindowFlagsNoScrollWithMouse)
	imgui.PopStyleVarV(2)

	avail := imgui.ContentRegionAvail()
	statusH := imgui.FrameHeight() + a.px(10)
	bodyH := avail.Y - statusH

	a.sidebar(a.px(208), bodyH)
	imgui.SameLineV(0, 0)

	imgui.PushStyleVarVec2(imgui.StyleVarItemSpacing, v2(0, 0))
	imgui.BeginChildStrV("##main", v2(0, bodyH), 0, imgui.WindowFlagsNoScrollbar)
	imgui.PopStyleVar()
	consoleH := imgui.FrameHeight() + a.px(12)
	if a.ui.consoleOpen {
		consoleH = max(a.px(250), bodyH*0.32)
	}
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(20), a.px(16)))
	imgui.BeginChildStrV("##content", v2(0, bodyH-consoleH), imgui.ChildFlagsAlwaysUseWindowPadding, imgui.WindowFlagsNoScrollbar)
	imgui.PopStyleVar()
	switch a.ui.view {
	case viewServices:
		a.servicesView()
	case viewCatalog:
		a.catalogView()
	case viewNetwork:
		a.networkView()
	case viewBackups:
		a.backupsView()
	case viewHealth:
		a.healthView()
	case viewSettings:
		a.settingsView()
	}
	imgui.EndChild()
	a.console(consoleH)
	imgui.EndChild()

	a.statusBar(statusH)

	a.modalWindow()
	a.paletteWindow()
	a.copyWindow()
	a.toastsWindow()
	imgui.End()

	// A logs stream nobody is looking at is stopped.
	if !a.ui.logsVisible && a.ui.logsJob != 0 {
		a.stop(a.ui.logsJob)
		a.ui.logsJob, a.ui.logsFor = 0, ""
	}
}

func (a *app) shortcuts() {
	if imgui.IsKeyChordPressed(imgui.KeyChord(imgui.ModCtrl | imgui.KeyK)) {
		a.openPalette()
	}
	if imgui.IsKeyPressedBool(imgui.KeyF5) {
		go a.refresh()
	}
	if imgui.IsKeyChordPressed(imgui.KeyChord(imgui.ModCtrl|imgui.KeyGraveAccent)) || imgui.IsKeyChordPressed(imgui.KeyChord(imgui.ModCtrl|imgui.KeyJ)) {
		a.ui.consoleOpen = !a.ui.consoleOpen
	}
}

// ── sidebar ───────────────────────────────────────────────────────────────────

func (a *app) sidebar(w, h float32) {
	imgui.PushStyleColorVec4(imgui.ColChildBg, vec4(cBgDark))
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(12), a.px(16)))
	imgui.PushStyleVarVec2(imgui.StyleVarItemSpacing, v2(a.px(8), a.px(4)))
	imgui.BeginChildStrV("##sidebar", v2(w, h), imgui.ChildFlagsAlwaysUseWindowPadding, imgui.WindowFlagsNoScrollbar)

	// Brand.
	imgui.Indent()
	withFont(nil, 22, func() { textC(cAccent, glyphCubes) })
	imgui.SameLineV(0, a.px(10))
	withFont(fontBold, 22, func() { text("homelab") })
	imgui.Unindent()
	imgui.Dummy(v2(0, a.px(14)))

	installed, available := 0, 0
	for _, s := range a.v.services {
		if s.Installed {
			installed++
		} else {
			available++
		}
	}
	counts := map[viewID]string{viewServices: strconv.Itoa(installed), viewCatalog: strconv.Itoa(available), viewBackups: strconv.Itoa(len(a.v.backups))}
	itemH := imgui.FrameHeight() + a.px(8)
	for i, it := range views {
		id := viewID(i)
		active := a.ui.view == id
		p := imgui.CursorScreenPos()
		width := imgui.ContentRegionAvail().X
		if imgui.InvisibleButtonV("##nav"+it.label, v2(width, itemH), imgui.ButtonFlagsNone) {
			a.ui.view = id
		}
		hovered := imgui.IsItemHovered()
		if hovered {
			imgui.SetMouseCursor(imgui.MouseCursorHand)
		}
		dl := imgui.WindowDrawList()
		switch {
		case active:
			dl.AddRectFilledV(p, v2(p.X+width, p.Y+itemH), u32(cActive.alpha(0.55)), a.px(8), imgui.DrawFlagsNone)
			dl.AddRectFilledV(v2(p.X, p.Y+a.px(8)), v2(p.X+a.px(3), p.Y+itemH-a.px(8)), u32(cPrimary), a.px(2), imgui.DrawFlagsNone)
		case hovered:
			dl.AddRectFilledV(p, v2(p.X+width, p.Y+itemH), u32(cHover), a.px(8), imgui.DrawFlagsNone)
		}
		ty := p.Y + (itemH-imgui.TextLineHeight())/2
		gc, tc := cMuted, cMuted
		if active {
			gc, tc = cPrimary, cText
		} else if hovered {
			tc = cText
		}
		dl.AddTextVec2(v2(p.X+a.px(14), ty), u32(gc), it.glyph)
		dl.AddTextVec2(v2(p.X+a.px(44), ty), u32(tc), it.label)
		if c := counts[id]; c != "" && c != "0" {
			cw := imgui.CalcTextSize(c).X
			dl.AddTextVec2(v2(p.X+width-cw-a.px(12), ty), u32(cDim), c)
		}
		if i == 0 {
			continue
		}
	}

	// Bottom: core summary and the palette hint.
	foot := imgui.FrameHeight()*2 + a.px(26)
	imgui.SetCursorPosY(h - foot - a.px(16))
	imgui.Separator()
	imgui.Dummy(v2(0, a.px(6)))
	up, total := coreCounts(a.v.core)
	c := cSuccess
	switch {
	case total == 0:
		c = cDim
	case up < total:
		c = cWarning
	}
	dotF(c)
	imgui.SameLineV(0, a.px(8))
	imgui.AlignTextToFramePadding()
	textC(cMuted, fmt.Sprintf("Core  %d/%d up", up, total))
	tip("Core containers running (Caddy, Tailscale and extensions)")
	imgui.Dummy(v2(0, a.px(4)))
	if button(glyphSearch+"  Commands   Ctrl+K##pal", toneGhost, false) {
		a.openPalette()
	}
	imgui.EndChild()
	imgui.PopStyleVarV(2)
	imgui.PopStyleColor()
}

// coreCounts counts the core containers that exist and those running.
func coreCounts(core []ContainerState) (up, total int) {
	for _, c := range core {
		if c.State == "" {
			continue
		}
		total++
		if c.State == "running" {
			up++
		}
	}
	return up, total
}

// viewHeader draws a view title with an optional subtitle.
func (a *app) viewHeader(glyph, title, sub string) {
	withFont(nil, 22, func() { textC(cPrimary, glyph) })
	imgui.SameLineV(0, a.px(12))
	heading(title, 22)
	if sub != "" {
		imgui.SameLineV(0, a.px(12))
		imgui.SetCursorPosY(imgui.CursorPosY() + a.px(5))
		textC(cMuted, sub)
	}
	imgui.Dummy(v2(0, a.px(8)))
}

// ── targets ───────────────────────────────────────────────────────────────────

func (a *app) globalTarget() actions.Target { return actions.GlobalTarget(a.v.layers) }
func (a *app) coreTarget() actions.Target   { return actions.CoreTarget(a.v.layers) }

func (a *app) service(name string) (service.Service, bool) {
	for _, s := range a.v.services {
		if s.Name == name {
			return s, true
		}
	}
	return service.Service{}, false
}

func (a *app) busy() bool {
	return slices.ContainsFunc(a.v.jobs, func(j job) bool { return j.Running && !j.Stream })
}

// blocked reports whether act must wait for the running job: one change at a
// time; read-only inspection and streams may run alongside.
func (a *app) blocked(act actions.Action) bool {
	return a.busy() && act.Group != actions.GroupInspect && !act.Stream
}

// ── running actions ──────────────────────────────────────────────────────────

// request is what clicking an action does: open its logs tab, its form or
// confirmation, or run it. quick is a toolbar/palette click, which runs with
// defaults when nothing is required.
func (a *app) request(act actions.Action, t actions.Target, ctx string, quick bool, preset actions.Inputs) {
	if act.Stream && onToolbar(act) && t.Scope == actions.Service && t.Name != "" && quick && preset == nil {
		a.ui.view, a.ui.selected, a.ui.tabReq = viewServices, t.Name, tabLogs
		return
	}
	if needsForm(act, quick) || preset != nil {
		a.openModal(act, t, ctx, preset)
		return
	}
	a.execute(act, t, ctx, defaultInputs(act))
}

func (a *app) execute(act actions.Action, t actions.Target, ctx string, in actions.Inputs) int {
	args := act.Build(t, in)
	label := act.Label
	if ctx != "" {
		label += " · " + ctx
	}
	if act.Interactive {
		if !a.openTerminal(label, args) {
			a.ui.copyCmd = shellQuote(append(slices.Clone(a.opt.CLI), args...))
			a.ui.copyOpen = true
		}
		return 0
	}
	id := a.start(label, args, nil, act.Stream, nil)
	a.ui.consoleJob = id
	// Health shows its checks' output inline; elsewhere output opens the console.
	if act.Stream || act.Group == actions.GroupInspect && a.ui.view != viewHealth {
		a.ui.consoleOpen = true
	}
	return id
}

// actionButton renders one action as a button and handles its click.
// quick: see request. iconOnly shows the glyph with the label as tooltip.
func (a *app) actionButton(act actions.Action, t actions.Target, ctx string, quick, iconOnly bool, fl *flow) {
	label := Glyph(act.Icon) + "  " + act.Label
	if !quick && (len(act.Inputs) > 0 || act.Danger != actions.None) {
		label += "…"
	}
	if iconOnly {
		label = Glyph(act.Icon)
	}
	label += "##" + act.ID + "|" + strings.Join(t.Names, ",") + t.Name
	tn := toneForIcon(act.Icon)
	if act.Danger == actions.TypeName || act.Group == actions.GroupDanger {
		tn = toneDanger
	} else if act.Danger == actions.Confirm {
		tn = toneWarning
	} else if !quick && !iconOnly && tn != toneDanger {
		tn = toneNormal
	}
	if fl != nil {
		fl.next(label, 0)
	}
	disabled := a.blocked(act)
	if button(label, tn, disabled) {
		a.request(act, t, ctx, quick, nil)
	}
	hint := act.Label
	if act.Help != "" {
		hint += " — " + act.Help
	}
	hint += "\n\nhomelab " + shellQuote(act.Build(t, nil))
	switch {
	case disabled:
		hint += "\n\nWaiting for the running action to finish."
	case quick && len(act.Inputs) > 0 && !needsForm(act, true):
		hint += "\n\nRight-click for options."
	}
	tip(hint)
	if quick && len(act.Inputs) > 0 && !disabled && imgui.IsItemClickedV(imgui.MouseButtonRight) {
		a.openModal(act, t, ctx, nil)
	}
}

// actionGroups renders actions under their group headings as wrapped buttons.
func (a *app) actionGroups(list []actions.Action, t actions.Target, ctx string) {
	for _, grp := range grouped(list) {
		section(groupGlyph(grp.Name), grp.Name)
		fl := newFlow()
		for _, act := range grp.Actions {
			a.actionButton(act, t, ctx, false, false, fl)
		}
		imgui.Dummy(v2(0, a.px(4)))
	}
}

func groupGlyph(group string) string {
	switch group {
	case actions.GroupLifecycle:
		return Glyph("play")
	case actions.GroupExposure:
		return Glyph("link")
	case actions.GroupInspect:
		return Glyph("eye")
	case actions.GroupMaintenance:
		return Glyph("wrench")
	case actions.GroupConfigure:
		return Glyph("gear")
	case actions.GroupDanger:
		return glyphWarn
	}
	return glyphGeneric
}

// toolbar renders quick buttons in a row.
func (a *app) toolbar(list []actions.Action, t actions.Target, ctx string) {
	fl := newFlow()
	imgui.PushStyleVarVec2(imgui.StyleVarFramePadding, v2(a.px(12), a.px(7)))
	for _, act := range list {
		a.actionButton(act, t, ctx, true, false, fl)
	}
	imgui.PopStyleVar()
}

// ── Services ──────────────────────────────────────────────────────────────────

func (a *app) servicesView() {
	var installed []service.Service
	running := 0
	for _, s := range a.v.services {
		if s.Installed {
			installed = append(installed, s)
			if serviceState(s) == stateRunning {
				running++
			}
		}
	}
	a.viewHeader(glyphServices, "Services", fmt.Sprintf("%d installed · %d running", len(installed), running))
	if a.v.discoverErr != "" {
		textWrapped(cError, glyphWarn+"  "+a.v.discoverErr)
	}

	listW := a.px(340)
	imgui.PushStyleVarVec2(imgui.StyleVarItemSpacing, v2(a.px(8), a.px(8)))
	if card("##svclist", listW, 0, 0) {
		a.serviceList(installed)
	}
	endCard()
	imgui.SameLineV(0, a.px(16))
	imgui.BeginChildStrV("##svcdetail", v2(0, 0), 0, 0)
	a.batchBar()
	if a.ui.selected == "" {
		a.stackDetail(installed)
	} else if s, ok := a.service(a.ui.selected); ok && s.Installed {
		a.serviceDetail(s)
	} else {
		a.ui.selected = ""
	}
	imgui.EndChild()
	imgui.PopStyleVar()
}

func (a *app) serviceList(installed []service.Service) {
	imgui.SetNextItemWidth(-1)
	imgui.InputTextWithHint("##filter", glyphSearch+"  Search services or layers", &a.ui.filter, 0, nil)

	var visible []service.Service
	for _, s := range installed {
		if matchesFilter(s, a.ui.filter) {
			visible = append(visible, s)
		}
	}
	all := len(visible) > 0
	for _, s := range visible {
		all = all && a.ui.checked[s.Name]
	}
	if imgui.Checkbox("##all", &all) {
		for _, s := range visible {
			if all {
				a.ui.checked[s.Name] = true
			} else {
				delete(a.ui.checked, s.Name)
			}
		}
	}
	tip("Select every visible service for a batch action")
	imgui.SameLine()
	textC(cMuted, fmt.Sprintf("%d shown", len(visible)))

	imgui.PushStyleVarVec2(imgui.StyleVarItemSpacing, v2(a.px(8), a.px(2)))
	imgui.BeginChildStrV("##rows", v2(0, 0), 0, 0)
	rowH := imgui.FrameHeight() + a.px(10)

	// The pinned stack row.
	a.listRow("##stack", a.ui.selected == "", rowH, func() {
		textC(cAccent, glyphStack)
		imgui.SameLineV(0, a.px(10))
		withFont(fontBold, 0, func() { text("Stack") })
		imgui.SameLineV(0, a.px(8))
		textC(cMuted, "all services")
	}, func() { a.ui.selected = "" })
	imgui.Dummy(v2(0, a.px(2)))

	for _, s := range visible {
		name := s.Name
		a.listRow("##row"+name, a.ui.selected == name, rowH, func() {
			on := a.ui.checked[name]
			if imgui.Checkbox("##chk"+name, &on) {
				if on {
					a.ui.checked[name] = true
				} else {
					delete(a.ui.checked, name)
				}
			}
			imgui.SameLineV(0, a.px(8))
			dotF(stateColor(serviceState(s)))
			imgui.SameLineV(0, a.px(8))
			text(name)
			if len(s.Layers) > 0 {
				var gl []string
				for _, l := range s.Layers {
					gl = append(gl, layerGlyph(string(l)))
				}
				badge := strings.Join(gl, " ")
				imgui.SameLine()
				rightAlign(imgui.CalcTextSize(badge).X + a.px(4))
				textC(cAccent.alpha(0.85), badge)
				tip("Exposed on: " + layerList(s.Layers))
			}
		}, func() { a.ui.selected = name })
	}
	if len(visible) == 0 {
		imgui.Dummy(v2(0, a.px(12)))
		if len(installed) == 0 {
			textWrapped(cMuted, "No services installed yet. Install one from the Catalog.")
			if button(glyphCatalog+"  Open catalog", tonePrimary, false) {
				a.ui.view = viewCatalog
			}
		} else {
			textWrapped(cMuted, "Nothing matches the search.")
		}
	}
	imgui.EndChild()
	imgui.PopStyleVar()
}

func layerList(ls []service.LayerName) string {
	var out []string
	for _, l := range ls {
		out = append(out, string(l))
	}
	return strings.Join(out, ", ")
}

// listRow is a full-width selectable row whose content may hold widgets.
func (a *app) listRow(id string, selected bool, h float32, content func(), onClick func()) {
	start := imgui.CursorPos()
	imgui.PushStyleColorVec4(imgui.ColHeader, vec4(cActive.alpha(0.6)))
	imgui.PushStyleColorVec4(imgui.ColHeaderHovered, vec4(cHover))
	imgui.PushStyleColorVec4(imgui.ColHeaderActive, vec4(cActive))
	if imgui.SelectableBoolV(id, selected, imgui.SelectableFlagsAllowOverlap, v2(0, h)) {
		onClick()
	}
	imgui.PopStyleColorV(3)
	end := imgui.CursorPos()
	imgui.SetCursorPos(v2(start.X+a.px(8), start.Y+(h-imgui.FrameHeight())/2))
	imgui.PushIDStr(id)
	imgui.AlignTextToFramePadding()
	content()
	imgui.PopID()
	imgui.SetCursorPos(end)
	imgui.Dummy(v2(0, 0))
}

func stateColor(s runState) rgba {
	switch s {
	case stateRunning:
		return cSuccess
	case statePartial:
		return cWarning
	case stateStopped:
		return cDim
	}
	return cBlue
}

// batchBar offers the Multi actions for the checked services.
func (a *app) batchBar() {
	var names []string
	for _, s := range a.v.services {
		if a.ui.checked[s.Name] && s.Installed {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	t := multiTarget(a.v.services, names, a.v.layers)
	ctx := strconv.Itoa(len(names)) + " services"
	if len(names) == 1 {
		ctx = names[0]
	}
	imgui.PushStyleColorVec4(imgui.ColBorder, vec4(cAccent.alpha(0.6)))
	if card("##batch", 0, 0, imgui.ChildFlagsAutoResizeY) {
		textC(cAccent, glyphStack)
		imgui.SameLineV(0, a.px(8))
		withFont(fontBold, 0, func() { text(fmt.Sprintf("%d selected", len(names))) })
		imgui.SameLineV(0, a.px(8))
		textC(cMuted, strings.Join(names, ", "))
		imgui.SameLine()
		rightAlign(buttonWidth(glyphClose + "  Clear"))
		if button(glyphClose+"  Clear##clearsel", toneGhost, false) {
			a.ui.checked = map[string]bool{}
		}
		fl := newFlow()
		for _, act := range actions.For(t) {
			a.actionButton(act, t, ctx, true, false, fl)
		}
	}
	endCard()
	imgui.PopStyleColor()
	imgui.Dummy(v2(0, a.px(6)))
}

// stackDetail is the Global target: every stack-wide action.
func (a *app) stackDetail(installed []service.Service) {
	t := a.globalTarget()
	var run, part, stop, exposed int
	for _, s := range installed {
		switch serviceState(s) {
		case stateRunning:
			run++
		case statePartial:
			part++
		default:
			stop++
		}
		if len(s.Layers) > 0 {
			exposed++
		}
	}
	withFont(nil, 20, func() { textC(cAccent, glyphStack) })
	imgui.SameLineV(0, a.px(10))
	heading("Stack", 20)
	imgui.SameLineV(0, a.px(14))
	pill(fmt.Sprintf("%d running", run), cSuccess)
	imgui.SameLine()
	if part > 0 {
		pill(fmt.Sprintf("%d partial", part), cWarning)
		imgui.SameLine()
	}
	pill(fmt.Sprintf("%d stopped", stop), cDim)
	imgui.SameLine()
	pill(fmt.Sprintf("%d exposed", exposed), cAccent)
	textC(cMuted, "Actions over every installed service, or a group of them. Select a service on the left for its own controls.")
	imgui.Dummy(v2(0, a.px(4)))
	s := split(t)
	a.toolbar(s.Toolbar, t, "stack")
	imgui.BeginChildStrV("##stackacts", v2(0, 0), 0, 0)
	a.actionGroups(s.Rest, t, "stack")
	imgui.EndChild()
}

// ── service detail ───────────────────────────────────────────────────────────

func (a *app) serviceDetail(s service.Service) {
	t := actions.ServiceTarget(s, a.v.layers)
	name := s.Name
	if a.ui.addrFor != name {
		a.ui.addrFor = name
		a.resolveAddrs(name)
	}

	// Header.
	withFont(fontBold, 24, func() { text(name) })
	imgui.SameLineV(0, a.px(14))
	st := serviceState(s)
	pill(stateLabel(s), stateColor(st))
	if h := service.AggregateHealth(s.Containers); h != "" {
		imgui.SameLine()
		hc := cSuccess
		switch h {
		case "unhealthy":
			hc = cError
		case "starting":
			hc = cWarning
		}
		pill(h, hc)
	}
	for _, l := range s.Layers {
		imgui.SameLine()
		pill(layerGlyph(string(l))+" "+string(l), cAccent)
	}
	a.primaryLinks(name)
	imgui.Dummy(v2(0, a.px(2)))

	sf := split(t)
	a.toolbar(sf.Toolbar, t, name)
	imgui.Dummy(v2(0, a.px(4)))

	if imgui.BeginTabBar("##svctabs") {
		tabs := []string{Glyph("info") + "  Overview", layerGlyph("ts") + "  Network", Glyph("gear") + "  Config", Glyph("logs") + "  Logs", glyphGeneric + "  Actions"}
		for i, label := range tabs {
			flags := imgui.TabItemFlagsNone
			if a.ui.tabReq == i {
				flags = imgui.TabItemFlagsSetSelected
			}
			if imgui.BeginTabItemV(label+"###tab"+strconv.Itoa(i), nil, flags) {
				imgui.Dummy(v2(0, a.px(4)))
				imgui.BeginChildStrV("##tabbody"+strconv.Itoa(i), v2(0, 0), 0, 0)
				switch i {
				case tabOverview:
					a.overviewTab(s)
				case tabNetwork:
					a.networkTab(s, t, sf.Network)
				case tabConfig:
					a.configTab(name)
				case tabLogs:
					a.logsTab(s, t)
				case tabActions:
					if len(sf.Rest) == 0 {
						textC(cMuted, "No further actions for this service right now.")
					}
					a.actionGroups(sf.Rest, t, name)
				}
				imgui.EndChild()
				imgui.EndTabItem()
			}
		}
		a.ui.tabReq = -1
		imgui.EndTabBar()
	}
}

// primaryLinks shows the first address of each layer under the header.
func (a *app) primaryLinks(name string) {
	addrs, ok := a.v.addrs[name]
	if !ok {
		return
	}
	first := true
	for _, la := range addrs {
		if len(la.Addrs) == 0 || la.Addrs[0].URL == "" {
			continue
		}
		if first {
			first = false
		} else {
			imgui.SameLineV(0, a.px(18))
		}
		textC(cAccent, layerGlyph(la.Layer))
		imgui.SameLineV(0, a.px(6))
		a.link(la.Addrs[0].URL, la.Addrs[0].URL)
	}
}

func (a *app) overviewTab(s service.Service) {
	section(glyphCubes, "Containers")
	if len(s.Containers) == 0 {
		textC(cMuted, "No containers yet — press Up to create them.")
	} else {
		cs := slices.Clone(s.Containers)
		sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
		a.containerTable(cs)
	}
	section(Glyph("link"), "Addresses")
	addrs, ok := a.v.addrs[s.Name]
	switch {
	case len(s.Layers) == 0:
		textC(cMuted, "Not exposed on any layer. Turn one on in the Network tab.")
	case !ok:
		spinner(a.px(6), cPrimary)
		imgui.SameLine()
		textC(cMuted, "Resolving addresses…")
	default:
		a.addressTable(addrs)
	}
	if len(s.HostPorts) > 0 {
		section(Glyph("hash"), "Host ports")
		withFont(fontMono, 0, func() { text(strings.Join(s.HostPorts, "   ")) })
	}
}

func (a *app) containerTable(cs []docker.ContainerDetail) {
	flags := imgui.TableFlagsRowBg | imgui.TableFlagsBordersInnerH | imgui.TableFlagsPadOuterX | imgui.TableFlagsSizingStretchProp
	if !imgui.BeginTableV("##containers", 5, flags, v2(0, 0), 0) {
		return
	}
	imgui.TableSetupColumnV("Container", imgui.TableColumnFlagsWidthStretch, 3, 0)
	imgui.TableSetupColumnV("State", imgui.TableColumnFlagsWidthStretch, 1.4, 0)
	imgui.TableSetupColumnV("Health", imgui.TableColumnFlagsWidthStretch, 1.2, 0)
	imgui.TableSetupColumnV("Ports", imgui.TableColumnFlagsWidthStretch, 2.4, 0)
	imgui.TableSetupColumnV("Up since", imgui.TableColumnFlagsWidthStretch, 1.4, 0)
	imgui.TableHeadersRow()
	for _, c := range cs {
		imgui.TableNextRow()
		imgui.TableNextColumn()
		text(c.Name)
		imgui.TableNextColumn()
		sc := cDim
		if c.State == "running" {
			sc = cSuccess
		} else if c.State == "restarting" {
			sc = cWarning
		}
		dot(sc)
		imgui.SameLineV(0, a.px(6))
		textC(sc, c.State)
		imgui.TableNextColumn()
		switch c.Health {
		case "healthy":
			textC(cSuccess, c.Health)
		case "unhealthy":
			textC(cError, c.Health)
		case "":
			textC(cDim, "—")
		default:
			textC(cWarning, c.Health)
		}
		imgui.TableNextColumn()
		if len(c.Ports) == 0 {
			textC(cDim, "—")
		} else {
			textC(cMuted, strings.Join(c.Ports, ", "))
		}
		imgui.TableNextColumn()
		if c.StartedAt.IsZero() || c.State != "running" {
			textC(cDim, "—")
		} else {
			textC(cMuted, ago(c.StartedAt))
			tip(c.StartedAt.Local().Format(time.RFC1123))
		}
		if c.RestartCount > 0 {
			imgui.SameLine()
			textC(cWarning, fmt.Sprintf("(%d restarts)", c.RestartCount))
		}
	}
	imgui.EndTable()
}

func (a *app) addressTable(addrs []layerAddrs) {
	flags := imgui.TableFlagsRowBg | imgui.TableFlagsBordersInnerH | imgui.TableFlagsPadOuterX
	if !imgui.BeginTableV("##addrs", 2, flags, v2(0, 0), 0) {
		return
	}
	imgui.TableSetupColumnV("Layer", imgui.TableColumnFlagsWidthFixed, a.px(170), 0)
	imgui.TableSetupColumnV("Address", imgui.TableColumnFlagsWidthStretch, 0, 0)
	for _, la := range addrs {
		imgui.TableNextRow()
		imgui.TableNextColumn()
		textC(cAccent, layerGlyph(la.Layer))
		imgui.SameLineV(0, a.px(8))
		text(la.Layer)
		tip(la.Label)
		imgui.TableNextColumn()
		if len(la.Addrs) == 0 {
			textC(cDim, "no address yet")
		}
		for i, ad := range la.Addrs {
			if ad.URL != "" {
				a.link(ad.URL, ad.URL)
			}
			if ad.Note != "" {
				if ad.URL != "" {
					imgui.SameLine()
				}
				textC(cMuted, "("+ad.Note+")")
			}
			_ = i
		}
	}
	imgui.EndTable()
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// networkTab: one switch per layer, then the remaining exposure actions.
func (a *app) networkTab(s service.Service, t actions.Target, exposure []actions.Action) {
	section(Glyph("link"), "Layers")
	flags := imgui.TableFlagsRowBg | imgui.TableFlagsBordersInnerH | imgui.TableFlagsPadOuterX
	if imgui.BeginTableV("##layers", 3, flags, v2(0, 0), 0) {
		imgui.TableSetupColumnV("on", imgui.TableColumnFlagsWidthFixed, a.px(64), 0)
		imgui.TableSetupColumnV("layer", imgui.TableColumnFlagsWidthFixed, a.px(260), 0)
		imgui.TableSetupColumnV("addr", imgui.TableColumnFlagsWidthStretch, 0, 0)
		addrs := map[string][]layerAddrs{}
		for _, la := range a.v.addrs[s.Name] {
			addrs[la.Layer] = append(addrs[la.Layer], la)
		}
		for _, l := range a.opt.AllLayers {
			ln := l.Name()
			on, act := layerToggle(t, ln)
			imgui.TableNextRow()
			imgui.TableNextColumn()
			disabled := act == nil || a.blocked(*act)
			if toggle("##tg"+ln, on, disabled) && act != nil {
				a.request(*act, t, s.Name, true, nil)
			}
			switch {
			case act != nil:
				tip(act.Label + " — " + act.Help)
			case !slices.Contains(a.v.layers, ln):
				tip("The " + ln + " extension is off. Turn it on in the Network view.")
			}
			imgui.TableNextColumn()
			imgui.AlignTextToFramePadding()
			c := cMuted
			if on {
				c = cAccent
			}
			textC(c, layerGlyph(ln))
			imgui.SameLineV(0, a.px(8))
			text(ln)
			imgui.SameLineV(0, a.px(8))
			textC(cMuted, l.Label())
			imgui.TableNextColumn()
			imgui.AlignTextToFramePadding()
			switch {
			case !slices.Contains(a.v.layers, ln):
				textC(cDim, "extension off")
			case !on:
				textC(cDim, "not exposed")
			default:
				shown := false
				for _, la := range addrs[ln] {
					for _, ad := range la.Addrs {
						if ad.URL != "" {
							if shown {
								imgui.SameLineV(0, a.px(14))
							}
							a.link(ad.URL, ad.URL)
							shown = true
						}
					}
				}
				if !shown {
					textC(cMuted, "resolving…")
				}
			}
		}
		imgui.EndTable()
	}
	toggles := layerToggleIDs(a.v.layers)
	rest := filterBy(exposure, func(x actions.Action) bool { return !slices.Contains(toggles, x.ID) })
	if len(rest) > 0 {
		section(Glyph("wrench"), "More")
		fl := newFlow()
		for _, act := range rest {
			a.actionButton(act, t, s.Name, false, false, fl)
		}
	}
}

func (a *app) logsTab(s service.Service, t actions.Target) {
	a.ui.logsVisible = true
	if a.ui.logsFor != s.Name {
		if a.ui.logsJob != 0 {
			a.stop(a.ui.logsJob)
		}
		a.ui.logsFor, a.ui.logsJob = s.Name, a.startLogs(t, s.Name)
	}
	var j *job
	for i := range a.v.jobs {
		if a.v.jobs[i].ID == a.ui.logsJob {
			j = &a.v.jobs[i]
		}
	}
	running := j != nil && j.Running
	if running {
		if button(Glyph("stop")+"  Stop##logstop", toneDanger, false) {
			a.stop(a.ui.logsJob)
		}
		imgui.SameLine()
		spinner(a.px(6), cSuccess)
		imgui.SameLine()
		textC(cMuted, "following")
	} else {
		if button(Glyph("play")+"  Follow##logfollow", toneSuccess, false) {
			a.ui.logsJob = a.startLogs(t, s.Name)
		}
	}
	imgui.SameLine()
	if button(glyphClear+"  Clear##logclear", toneNormal, j == nil) {
		a.clearJob(a.ui.logsJob)
	}
	imgui.SameLine()
	if button(glyphCopy+"  Copy##logcopy", toneNormal, j == nil) && j != nil {
		imgui.SetClipboardText(strings.Join(j.Lines, "\n"))
		a.notify("Logs copied", toastInfo)
	}
	imgui.SameLine()
	textC(cDim, "last 200 lines, then live")
	var lines []string
	if j != nil {
		lines = j.Lines
		if j.Err != "" {
			lines = append(slices.Clone(lines), "error: "+j.Err)
		}
	}
	logLines("##svclogs", lines, 0)
}

// startLogs streams the target's logs action (follow, last 200 lines).
func (a *app) startLogs(t actions.Target, ctx string) int {
	for _, act := range actions.For(t) {
		if act.Stream && onToolbar(act) {
			args := act.Build(t, actions.Inputs{"follow": "true", "tail": "200"})
			return a.start("Logs · "+ctx, args, nil, true, nil)
		}
	}
	return 0
}

// ── config form ──────────────────────────────────────────────────────────────

func (a *app) configTab(name string) {
	if a.ui.cfgFor != name {
		a.ui.cfgFor, a.ui.cfg, a.ui.cfgGen = name, nil, 0
		a.loadSetup(name)
	}
	if res, ok := a.v.setup[name]; ok && res.Gen != a.ui.cfgGen {
		a.ui.cfgGen, a.ui.cfg = res.Gen, res.Form.clone()
	}
	res := a.v.setup[name]
	a.setupEditor("svc", name, a.ui.cfg, res.Err, func() { a.ui.cfgFor = "" })
}

// setupEditor renders a setup form with Save/Revert. reload forgets the
// edited copy so the next frame loads it again.
func (a *app) setupEditor(id, svc string, f *setupForm, loadErr string, reload func()) {
	if loadErr != "" {
		textWrapped(cError, glyphWarn+"  "+loadErr)
		if button(Glyph("refresh")+"  Retry##"+id, toneNormal, false) {
			reload()
		}
		return
	}
	if f == nil {
		spinner(a.px(6), cPrimary)
		imgui.SameLine()
		textC(cMuted, "Loading settings…")
		return
	}
	if len(f.Vars) == 0 && len(f.Secrets) == 0 {
		textC(cMuted, "This service declares no settings.")
		return
	}
	// Save bar.
	dirty := f.Dirty()
	if button(glyphSave+"  Save##save"+id, toneSuccess, !dirty || a.busy()) {
		args, stdin := f.Save()
		ctx := svc
		if ctx == "" {
			ctx = "homelab"
		}
		id := a.start("Save settings · "+ctx, args, stdin, false, func(ok bool) {
			if ok {
				a.loadSetup(svc)
			}
		})
		a.ui.consoleJob = id
	}
	tip("Runs homelab setup --set … ; secrets go over stdin (--secrets-stdin), never the command line")
	imgui.SameLine()
	if button(glyphUndo+"  Revert##rev"+id, toneNormal, !dirty) {
		reload()
	}
	imgui.SameLine()
	if button(Glyph("refresh")+"  Reload##rl"+id, toneGhost, false) {
		reload()
	}
	if miss := f.Missing(); len(miss) > 0 {
		imgui.SameLineV(0, a.px(16))
		textC(cWarning, glyphWarn+"  Required: "+strings.Join(miss, ", "))
	} else if dirty {
		imgui.SameLineV(0, a.px(16))
		textC(cWarning, "Unsaved changes")
	}

	labelW := a.px(260)
	row := func(name, desc string, required bool, changed bool) {
		imgui.TableNextRow()
		imgui.TableNextColumn()
		imgui.AlignTextToFramePadding()
		withFont(fontMono, 0, func() { text(name) })
		if required {
			imgui.SameLineV(0, a.px(4))
			textC(cError, "*")
			tip("Required")
		}
		if changed {
			imgui.SameLineV(0, a.px(6))
			dot(cWarning)
			tip("Changed")
		}
		if desc != "" {
			imgui.PushTextWrapPosV(imgui.CursorPosX() + labelW - a.px(12))
			textC(cMuted, desc)
			imgui.PopTextWrapPos()
		}
		imgui.TableNextColumn()
	}
	flags := imgui.TableFlagsRowBg | imgui.TableFlagsBordersInnerH | imgui.TableFlagsPadOuterX
	if len(f.Vars) > 0 {
		section(Glyph("tag"), "Variables")
		if imgui.BeginTableV("##vars"+id, 2, flags, v2(0, 0), 0) {
			imgui.TableSetupColumnV("name", imgui.TableColumnFlagsWidthFixed, labelW, 0)
			imgui.TableSetupColumnV("value", imgui.TableColumnFlagsWidthStretch, 0, 0)
			for i := range f.Vars {
				v := &f.Vars[i]
				row(v.Name, v.Description, v.Required, v.Value != v.Orig)
				imgui.SetNextItemWidth(-a.px(44))
				imgui.InputTextWithHint("##v"+v.Name, "empty", &v.Value, 0, nil)
				if v.Value != v.Orig {
					imgui.SameLine()
					if button(glyphUndo+"##u"+v.Name, toneGhost, false) {
						v.Value = v.Orig
					}
					tip("Revert to " + strconv.Quote(v.Orig))
				}
			}
			imgui.EndTable()
		}
	}
	if len(f.Secrets) > 0 {
		section(glyphKey, "Secrets — stored in the system keyring")
		if imgui.BeginTableV("##secrets"+id, 2, flags, v2(0, 0), 0) {
			imgui.TableSetupColumnV("name", imgui.TableColumnFlagsWidthFixed, labelW, 0)
			imgui.TableSetupColumnV("value", imgui.TableColumnFlagsWidthStretch, 0, 0)
			for i := range f.Secrets {
				s := &f.Secrets[i]
				row(s.Name, s.Description, s.Required, strings.TrimSpace(s.Value) != "")
				st := s.status()
				sc := cDim
				switch st {
				case "set":
					sc = cSuccess
				case "generated":
					sc = cAccent
				case "will change":
					sc = cWarning
				case "unset":
					if s.Required {
						sc = cError
					}
				}
				pill(st, sc)
				imgui.SameLine()
				hint := "type a new value"
				if !s.Set {
					hint = "type a value"
				}
				if s.Gen && !s.Set {
					hint = "leave empty to generate"
				}
				fl := imgui.InputTextFlagsPassword
				if s.Reveal {
					fl = 0
				}
				imgui.SetNextItemWidth(-a.px(44))
				imgui.InputTextWithHint("##s"+s.Name, hint, &s.Value, fl, nil)
				imgui.SameLine()
				eye := Glyph("eye")
				if s.Reveal {
					eye = "" // eye-slash
				}
				if button(eye+"##r"+s.Name, toneGhost, false) {
					s.Reveal = !s.Reveal
				}
				tip("Show / hide what you typed. Stored values are never shown.")
			}
			imgui.EndTable()
		}
	}
}

// ── Catalog ───────────────────────────────────────────────────────────────────

func (a *app) catalogView() {
	var avail []service.Service
	for _, s := range a.v.services {
		if !s.Installed {
			avail = append(avail, s)
		}
	}
	a.viewHeader(glyphCatalog, "Catalog", fmt.Sprintf("%d services ready to install", len(avail)))
	imgui.SetNextItemWidth(a.px(360))
	imgui.InputTextWithHint("##catfilter", glyphSearch+"  Search the catalog", &a.ui.catFilter, 0, nil)
	imgui.Dummy(v2(0, a.px(6)))

	imgui.BeginChildStrV("##catgrid", v2(0, 0), 0, 0)
	cardW, cardH := a.px(230), a.px(112)
	gap := a.px(12)
	cols := max(1, int((imgui.ContentRegionAvail().X+gap)/(cardW+gap)))
	n := 0
	for _, s := range avail {
		if !matchesFilter(s, a.ui.catFilter) {
			continue
		}
		if n%cols != 0 {
			imgui.SameLineV(0, gap)
		} else if n > 0 {
			imgui.Dummy(v2(0, gap-imgui.CurrentStyle().ItemSpacing().Y))
		}
		n++
		t := actions.ServiceTarget(s, a.v.layers)
		if card("##cat"+s.Name, cardW, cardH, 0) {
			withFont(nil, 20, func() { textC(cBlue, Glyph("box")) })
			imgui.SameLineV(0, a.px(10))
			withFont(fontBold, 17, func() { text(s.Name) })
			textC(cMuted, "from the catalog")
			imgui.SetCursorPosY(cardH - imgui.FrameHeight() - a.px(12))
			for i, act := range actions.For(t) {
				if i > 0 {
					imgui.SameLine()
				}
				a.actionButton(act, t, s.Name, true, false, nil)
			}
		}
		endCard()
	}
	if n == 0 {
		textC(cMuted, "Nothing in the catalog matches.")
	}
	imgui.EndChild()
}

// ── Network ───────────────────────────────────────────────────────────────────

func (a *app) networkView() {
	up, total := coreCounts(a.v.core)
	a.viewHeader(glyphNetwork, "Network", fmt.Sprintf("core %d/%d up · %d layers configured", up, total, len(a.v.layers)))
	imgui.BeginChildStrV("##netbody", v2(0, 0), 0, 0)

	// Core.
	t := a.coreTarget()
	if card("##core", 0, 0, imgui.ChildFlagsAutoResizeY) {
		withFont(nil, 18, func() { textC(cPrimary, Glyph("shield")) })
		imgui.SameLineV(0, a.px(10))
		heading("Core stack", 18)
		imgui.SameLineV(0, a.px(10))
		textC(cMuted, "Tailscale + Caddy + enabled extensions — every route goes through it")
		imgui.Dummy(v2(0, a.px(2)))
		for i, c := range a.v.core {
			if i > 0 {
				imgui.SameLineV(0, a.px(22))
			}
			sc, st := cSuccess, c.State
			switch {
			case st == "":
				sc, st = cDim, "absent"
			case st != "running":
				sc = cWarning
			}
			dot(sc)
			imgui.SameLineV(0, a.px(6))
			text(c.Name)
			imgui.SameLineV(0, a.px(6))
			textC(sc, st)
		}
		if len(a.v.core) == 0 {
			textC(cMuted, "Docker is not reachable — container state unknown.")
		}
		imgui.Dummy(v2(0, a.px(4)))
		s := split(t)
		a.toolbar(s.Toolbar, t, "core")
		a.actionGroups(s.Rest, t, "core")
	}
	endCard()
	imgui.Dummy(v2(0, a.px(10)))

	// Extensions.
	section(Glyph("link"), "Extensions")
	exposed := map[string]int{}
	for _, s := range a.v.services {
		for _, l := range s.Layers {
			exposed[string(l)]++
		}
	}
	flags := imgui.TableFlagsRowBg | imgui.TableFlagsBordersInnerH | imgui.TableFlagsPadOuterX
	if imgui.BeginTableV("##exts", 5, flags, v2(0, 0), 0) {
		imgui.TableSetupColumnV("Layer", imgui.TableColumnFlagsWidthFixed, a.px(250), 0)
		imgui.TableSetupColumnV("Enabled", imgui.TableColumnFlagsWidthFixed, a.px(90), 0)
		imgui.TableSetupColumnV("Container", imgui.TableColumnFlagsWidthFixed, a.px(170), 0)
		imgui.TableSetupColumnV("Services", imgui.TableColumnFlagsWidthFixed, a.px(80), 0)
		imgui.TableSetupColumnV("Actions", imgui.TableColumnFlagsWidthStretch, 0, 0)
		imgui.TableHeadersRow()
		states := map[string]string{}
		for _, c := range a.v.core {
			states[c.Name] = c.State
		}
		for _, e := range a.v.exts {
			lt := actions.LayerTarget(e.Name, e.Enabled, e.Running)
			imgui.TableNextRow()
			imgui.TableNextColumn()
			imgui.AlignTextToFramePadding()
			c := cMuted
			if e.Enabled {
				c = cAccent
			}
			textC(c, layerGlyph(e.Name))
			imgui.SameLineV(0, a.px(8))
			withFont(fontBold, 0, func() { text(e.Name) })
			imgui.SameLineV(0, a.px(8))
			textC(cMuted, e.Label)
			imgui.TableNextColumn()
			if e.Enabled {
				pill("on", cSuccess)
			} else {
				pill("off", cDim)
			}
			imgui.TableNextColumn()
			imgui.AlignTextToFramePadding()
			st := states[e.Container]
			sc := cDim
			if st == "running" {
				sc = cSuccess
			} else if st != "" {
				sc = cWarning
			} else {
				st = "absent"
			}
			dotF(sc)
			imgui.SameLineV(0, a.px(6))
			textC(sc, st)
			imgui.TableNextColumn()
			imgui.AlignTextToFramePadding()
			textC(cMuted, strconv.Itoa(exposed[e.Name]))
			imgui.TableNextColumn()
			for i, act := range actions.For(lt) {
				if i > 0 {
					imgui.SameLineV(0, a.px(4))
				}
				a.actionButton(act, lt, e.Name, true, true, nil)
			}
		}
		imgui.EndTable()
	}
	imgui.EndChild()
}

// ── Backups ───────────────────────────────────────────────────────────────────

func (a *app) backupsView() {
	a.viewHeader(glyphBackups, "Backups", fmt.Sprintf("%d in %s", len(a.v.backups), shortPath(filepath.Join(a.opt.Root, "backups"))))
	t := a.globalTarget()
	acts := filterBy(actions.For(t), func(x actions.Action) bool { return touchesCommand(x, "backup", "restore") })
	fl := newFlow()
	for _, act := range acts {
		a.actionButton(act, t, "stack", false, false, fl)
	}
	imgui.SameLine()
	if button(glyphFolder+"  Open folder##bkdir", toneGhost, false) {
		a.openURL(filepath.Join(a.opt.Root, "backups"))
	}
	textC(cMuted, "Secrets are not in backups — they stay in the keyring. After restoring on a new machine, run Configure on each service.")
	imgui.Dummy(v2(0, a.px(6)))
	if a.v.backupsErr != "" {
		textWrapped(cError, glyphWarn+"  "+a.v.backupsErr)
	}
	if len(a.v.backups) == 0 {
		if card("##nobk", 0, a.px(120), 0) {
			withFont(nil, 24, func() { textC(cDim, glyphBackups) })
			text("No backups yet.")
			textC(cMuted, "Back up a single service from its Actions tab, or everything with Back up all.")
		}
		endCard()
		return
	}
	var restore *actions.Action
	for _, act := range acts {
		if touchesCommand(act, "restore") {
			restore = &act
		}
	}
	flags := imgui.TableFlagsRowBg | imgui.TableFlagsBordersInnerH | imgui.TableFlagsPadOuterX | imgui.TableFlagsScrollY
	if imgui.BeginTableV("##backups", 4, flags, v2(0, 0), 0) {
		imgui.TableSetupScrollFreeze(0, 1)
		imgui.TableSetupColumnV("Created", imgui.TableColumnFlagsWidthFixed, a.px(230), 0)
		imgui.TableSetupColumnV("Services", imgui.TableColumnFlagsWidthStretch, 0, 0)
		imgui.TableSetupColumnV("Mode", imgui.TableColumnFlagsWidthFixed, a.px(90), 0)
		imgui.TableSetupColumnV("", imgui.TableColumnFlagsWidthFixed, a.px(200), 0)
		imgui.TableHeadersRow()
		for _, b := range a.v.backups {
			imgui.TableNextRow()
			imgui.TableNextColumn()
			imgui.AlignTextToFramePadding()
			textC(cAccent, glyphClock)
			imgui.SameLineV(0, a.px(8))
			text(b.Created.Local().Format("2006-01-02 15:04"))
			imgui.SameLineV(0, a.px(8))
			textC(cMuted, ago(b.Created))
			tip(b.Dir)
			imgui.TableNextColumn()
			imgui.AlignTextToFramePadding()
			if len(b.Services) == 0 {
				textC(cDim, "config only")
			} else {
				text(strings.Join(b.Services, ", "))
			}
			imgui.TableNextColumn()
			if b.Live {
				pill("live", cWarning)
				tip("Taken without stopping the service; files may be torn")
			} else {
				pill("stopped", cSuccess)
				tip("Each service was stopped while it was copied")
			}
			imgui.TableNextColumn()
			if restore != nil {
				if button(Glyph("restore")+"  Restore…##r"+b.Dir, toneWarning, a.blocked(*restore)) {
					a.request(*restore, t, "stack", false, actions.Inputs{"backup": b.Dir})
				}
				tip(restore.Help)
				imgui.SameLine()
			}
			if button(glyphFolder+"##o"+b.Dir, toneGhost, false) {
				a.openURL(b.Dir)
			}
			tip("Open " + b.Dir)
		}
		imgui.EndTable()
	}
}

func shortPath(p string) string {
	if home, err := filepath.Abs(os.Getenv("HOME")); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// ── Health ────────────────────────────────────────────────────────────────────

func (a *app) healthView() {
	a.viewHeader(glyphHealth, "Health", "")
	var run, attention, stopped int
	var problems []service.Service
	for _, s := range a.v.services {
		if !s.Installed {
			continue
		}
		h := service.AggregateHealth(s.Containers)
		switch {
		case serviceState(s) == statePartial || h == "unhealthy" && s.Running > 0:
			attention++
			problems = append(problems, s)
		case serviceState(s) == stateRunning:
			run++
		default:
			stopped++
		}
	}
	up, total := coreCounts(a.v.core)

	tiles := []struct {
		glyph, label, value string
		c                   rgba
	}{
		{glyphOK, "Running", strconv.Itoa(run), cSuccess},
		{glyphWarn, "Need attention", strconv.Itoa(attention), cWarning},
		{Glyph("pause"), "Stopped", strconv.Itoa(stopped), cDim},
		{Glyph("shield"), "Core containers", fmt.Sprintf("%d/%d", up, total), cPrimary},
	}
	gap := a.px(12)
	w := (imgui.ContentRegionAvail().X - 3*gap) / 4
	for i, tl := range tiles {
		if i > 0 {
			imgui.SameLineV(0, gap)
		}
		if card("##tile"+tl.label, w, a.px(92), 0) {
			textC(tl.c, tl.glyph)
			imgui.SameLineV(0, a.px(8))
			textC(cMuted, tl.label)
			withFont(fontBold, 30, func() { textC(tl.c, tl.value) })
		}
		endCard()
	}

	imgui.Dummy(v2(0, a.px(4)))
	section(glyphWarn, "Attention")
	if len(problems) == 0 && up == total {
		textC(cSuccess, glyphOK+"  Everything that is installed and started is running.")
	}
	if up < total {
		textC(cWarning, fmt.Sprintf("%s  %d core container(s) not running — see Network.", glyphWarn, total-up))
	}
	for _, s := range problems {
		dot(cWarning)
		imgui.SameLineV(0, a.px(8))
		withFont(fontBold, 0, func() { text(s.Name) })
		imgui.SameLineV(0, a.px(8))
		note := stateLabel(s)
		if h := service.AggregateHealth(s.Containers); h != "" {
			note += " · " + h
		}
		textC(cMuted, note)
		imgui.SameLineV(0, a.px(12))
		name := s.Name
		if button(glyphExternal+"  Open##h"+name, toneGhost, false) {
			a.ui.view, a.ui.selected = viewServices, name
		}
		t := actions.ServiceTarget(s, a.v.layers)
		for _, act := range actions.For(t) {
			if act.Icon == "doctor" {
				imgui.SameLine()
				a.actionButton(act, t, name, true, false, nil)
			}
		}
	}

	section(Glyph("doctor"), "Checks")
	gt, ct := a.globalTarget(), a.coreTarget()
	fl := newFlow()
	for _, pair := range []struct {
		t   actions.Target
		ctx string
	}{{gt, "stack"}, {ct, "core"}} {
		for _, act := range actions.For(pair.t) {
			if act.Group != actions.GroupInspect || act.Stream {
				continue
			}
			fl.next(Glyph(act.Icon)+"  "+act.Label, 0)
			before := a.ui.consoleJob
			a.actionButton(act, pair.t, pair.ctx, true, false, nil)
			if a.ui.consoleJob != before {
				a.ui.healthJob = a.ui.consoleJob
			}
		}
	}
	imgui.Dummy(v2(0, a.px(4)))
	for _, j := range a.v.jobs {
		if j.ID == a.ui.healthJob {
			a.jobHeader(j)
			logLines("##healthout", j.Lines, 0)
			return
		}
	}
	textC(cMuted, "Run a check to see its report here.")
}

// ── Settings ──────────────────────────────────────────────────────────────────

func (a *app) settingsView() {
	a.viewHeader(glyphSettings, "Settings", shortPath(a.opt.Root))
	t := a.globalTarget()
	conf := filterBy(actions.For(t), func(x actions.Action) bool { return x.Group == actions.GroupConfigure })
	fl := newFlow()
	for _, act := range conf {
		a.actionButton(act, t, "homelab", true, false, fl)
	}
	imgui.Dummy(v2(0, a.px(4)))
	if !a.ui.rootReq {
		a.ui.rootReq = true
		a.loadSetup("")
	}
	if res, ok := a.v.setup[""]; ok && res.Gen != a.ui.rootGen {
		a.ui.rootGen, a.ui.rootCfg = res.Gen, res.Form.clone()
	}
	imgui.BeginChildStrV("##rootcfg", v2(0, 0), 0, 0)
	a.setupEditor("root", "", a.ui.rootCfg, a.v.setup[""].Err, func() { a.ui.rootReq = false })
	imgui.EndChild()
}
