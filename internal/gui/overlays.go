//go:build gui

package gui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AllenDang/cimgui-go/imgui"

	"github.com/groot/homelab/internal/actions"
)

// ── action modal: inputs and confirmation ─────────────────────────────────────

type modalState struct {
	act    actions.Action
	t      actions.Target
	ctx    string
	values actions.Inputs
	typed  string
	open   bool // OpenPopup pending
	focus  bool
}

const modalID = "##action-modal"

func (a *app) openModal(act actions.Action, t actions.Target, ctx string, preset actions.Inputs) {
	vals := defaultInputs(act)
	for k, v := range preset {
		vals[k] = v
	}
	for _, in := range act.Inputs {
		if in.Kind == actions.Choice && in.Source != "" {
			a.loadChoices(in) // refreshed every time the form opens
		}
	}
	a.ui.modal = &modalState{act: act, t: t, ctx: ctx, values: vals, open: true, focus: true}
}

func (a *app) modalWindow() {
	m := a.ui.modal
	if m == nil {
		return
	}
	if m.open {
		imgui.OpenPopupStr(modalID)
		m.open = false
	}
	vp := imgui.MainViewport()
	center := v2(vp.Pos().X+vp.Size().X/2, vp.Pos().Y+vp.Size().Y*0.4)
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, v2(0.5, 0.5))
	imgui.SetNextWindowSizeV(v2(a.px(560), 0), imgui.CondAlways)
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(22), a.px(18)))
	imgui.PushStyleVarVec2(imgui.StyleVarItemSpacing, v2(a.px(8), a.px(10)))
	if !imgui.BeginPopupModalV(modalID, nil, imgui.WindowFlagsNoTitleBar|imgui.WindowFlagsAlwaysAutoResize|imgui.WindowFlagsNoSavedSettings) {
		imgui.PopStyleVarV(2)
		a.ui.modal = nil
		return
	}
	act := m.act
	danger := act.Danger != actions.None
	gc := cPrimary
	if danger {
		gc = cError
	}
	withFont(nil, 20, func() { textC(gc, Glyph(act.Icon)) })
	imgui.SameLineV(0, a.px(10))
	heading(act.Label, 20)
	if m.ctx != "" {
		imgui.SameLineV(0, a.px(10))
		imgui.SetCursorPosY(imgui.CursorPosY() + a.px(3))
		textC(cMuted, m.ctx)
	}
	if act.Help != "" {
		c := cMuted
		if danger {
			c = cWarning
		}
		textWrapped(c, act.Help)
	}

	run := false
	for i, in := range act.Inputs {
		key := in.Key
		label := in.Label
		if in.Required {
			label += " *"
		}
		switch in.Kind {
		case actions.Bool:
			on := m.values.Bool(key)
			if imgui.Checkbox(label+"##in"+key, &on) {
				if on {
					m.values[key] = "true"
				} else {
					m.values[key] = ""
				}
			}
		case actions.Choice:
			textC(cMuted, label)
			items := in.Choices
			loading, errText := false, ""
			if in.Source != "" {
				res, ok := a.v.choices[in.Source]
				items, errText, loading = res.Items, res.Err, !ok
			}
			preview := m.values[key]
			if preview == "" {
				preview = "— none —"
				if in.Source == actions.SourceGroups {
					preview = "every installed service"
				}
			} else if in.Source == actions.SourceBackups {
				preview = filepath.Base(preview)
			}
			imgui.SetNextItemWidth(-1)
			if imgui.BeginComboV("##in"+key, preview, 0) {
				if !in.Required {
					if imgui.SelectableBoolV(preview+"##none", m.values[key] == "", 0, v2(0, 0)) {
						m.values[key] = ""
					}
				}
				for _, it := range items {
					shown := it
					if in.Source == actions.SourceBackups {
						shown = filepath.Base(it)
					}
					if imgui.SelectableBoolV(shown+"##"+it, m.values[key] == it, 0, v2(0, 0)) {
						m.values[key] = it
					}
				}
				imgui.EndCombo()
			}
			switch {
			case loading:
				textC(cDim, "loading…")
			case errText != "":
				textC(cError, errText)
			case len(items) == 0:
				textC(cDim, "nothing to choose from")
			}
		default: // Text, Path
			textC(cMuted, label)
			imgui.SetNextItemWidth(-1)
			if m.focus && i == firstTextInput(act) {
				imgui.SetKeyboardFocusHere()
				m.focus = false
			}
			hint := in.Default
			if in.Kind == actions.Path && hint == "" {
				hint = "/path/to/dir"
			}
			v := m.values[key]
			if imgui.InputTextWithHint("##in"+key, hint, &v, imgui.InputTextFlagsEnterReturnsTrue, nil) {
				run = true
			}
			m.values[key] = v
		}
		if in.Help != "" {
			textWrapped(cDim, in.Help)
		}
	}

	token := act.Token(m.t)
	if act.Danger == actions.TypeName {
		imgui.Dummy(v2(0, a.px(2)))
		textC(cError, glyphWarn+"  This cannot be undone.")
		text("Type ")
		imgui.SameLineV(0, 0)
		withFont(fontMono, 0, func() { textC(cError, token) })
		imgui.SameLineV(0, 0)
		text(" to confirm:")
		imgui.SetNextItemWidth(-1)
		if m.focus && firstTextInput(act) < 0 {
			imgui.SetKeyboardFocusHere()
			m.focus = false
		}
		if imgui.InputTextWithHint("##typed", token, &m.typed, imgui.InputTextFlagsEnterReturnsTrue, nil) {
			run = true
		}
	}

	missing := missingInputs(act, m.values)
	args := act.Build(m.t, m.values)
	imgui.Dummy(v2(0, a.px(2)))
	imgui.PushStyleColorVec4(imgui.ColChildBg, vec4(cBgDark))
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(10), a.px(8)))
	imgui.BeginChildStrV("##cmd", v2(0, imgui.TextLineHeight()*2+a.px(16)), imgui.ChildFlagsAlwaysUseWindowPadding, 0)
	withFont(fontMono, 0, func() {
		imgui.PushTextWrapPosV(0)
		textC(cMuted, "$ homelab "+shellQuote(args))
		imgui.PopTextWrapPos()
	})
	imgui.EndChild()
	imgui.PopStyleVar()
	imgui.PopStyleColor()

	ok := len(missing) == 0 && (act.Danger != actions.TypeName || m.typed == token)
	if len(missing) > 0 {
		textC(cWarning, "Required: "+strings.Join(missing, ", "))
	}
	runLabel := Glyph(act.Icon) + "  " + act.Label
	if act.Interactive {
		runLabel += " in terminal"
	}
	cancelW := buttonWidth("Cancel")
	rightAlign(buttonWidth(runLabel) + cancelW + imgui.CurrentStyle().ItemSpacing().X)
	if button("Cancel##modalcancel", toneGhost, false) || imgui.IsKeyPressedBool(imgui.KeyEscape) {
		imgui.CloseCurrentPopup()
		a.ui.modal = nil
	}
	imgui.SameLine()
	tn := tonePrimary
	if danger {
		tn = toneDanger
	}
	blocked := a.blocked(act)
	if button(runLabel+"##modalrun", tn, !ok || blocked) || (run && ok && !blocked) {
		vals := m.values
		imgui.CloseCurrentPopup()
		a.ui.modal = nil
		a.execute(act, m.t, m.ctx, vals)
	}
	if blocked {
		tip("Waiting for the running action to finish")
	}
	imgui.EndPopup()
	imgui.PopStyleVarV(2)
}

func firstTextInput(act actions.Action) int {
	for i, in := range act.Inputs {
		if in.Kind == actions.Text || in.Kind == actions.Path {
			return i
		}
	}
	return -1
}

// ── command palette ───────────────────────────────────────────────────────────

type paletteState struct {
	open    bool
	pending bool // OpenPopup on the next paletteWindow call
	query   string
	sel     int
	focus   bool
	items   []paletteItem
}

const paletteID = "##palette"

func (a *app) currentTarget() (actions.Target, string) {
	switch a.ui.view {
	case viewServices:
		if s, ok := a.service(a.ui.selected); ok && a.ui.selected != "" {
			return actions.ServiceTarget(s, a.v.layers), s.Name
		}
	case viewNetwork:
		return a.coreTarget(), "core"
	}
	return a.globalTarget(), "stack"
}

func (a *app) openPalette() {
	t, name := a.currentTarget()
	items := paletteItems(t, name, a.globalTarget())
	// The core and every extension are always reachable from the palette.
	if t.Scope != actions.Core {
		targetActs := actions.For(a.coreTarget())
		for i := range targetActs {
			act := &targetActs[i]
			items = append(items, paletteItem{Action: *act, Target: a.coreTarget(), Context: "core"})
		}
	}
	for _, e := range a.v.exts {
		lt := actions.LayerTarget(e.Name, e.Enabled, e.Running)
		acts := actions.For(lt)
		for i := range acts {
			act := &acts[i]
			items = append(items, paletteItem{Action: *act, Target: lt, Context: e.Name})
		}
	}
	a.ui.palette = paletteState{open: true, pending: true, focus: true, items: items}
}

func (a *app) paletteWindow() {
	p := &a.ui.palette
	if !p.open {
		return
	}
	if p.pending {
		imgui.OpenPopupStr(paletteID)
		p.pending = false
	}
	vp := imgui.MainViewport()
	w := min(a.px(640), vp.Size().X-a.px(40))
	imgui.SetNextWindowPosV(v2(vp.Pos().X+vp.Size().X/2, vp.Pos().Y+a.px(90)), imgui.CondAlways, v2(0.5, 0))
	imgui.SetNextWindowSizeV(v2(w, 0), imgui.CondAlways)
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(12), a.px(12)))
	if !imgui.BeginPopupModalV(paletteID, nil, imgui.WindowFlagsNoTitleBar|imgui.WindowFlagsAlwaysAutoResize|imgui.WindowFlagsNoSavedSettings) {
		imgui.PopStyleVar()
		p.open = false
		return
	}
	if p.focus {
		imgui.SetKeyboardFocusHere()
		p.focus = false
	}
	imgui.PushStyleVarVec2(imgui.StyleVarFramePadding, v2(a.px(12), a.px(10)))
	imgui.SetNextItemWidth(-1)
	if imgui.InputTextWithHint("##pq", glyphSearch+"  Type an action — e.g. restart, logs, expose tor", &p.query, 0, nil) {
		p.sel = 0
	}
	imgui.PopStyleVar()
	hits := fuzzyFilter(p.items, p.query)
	if imgui.IsKeyPressedBool(imgui.KeyDownArrow) {
		p.sel = min(p.sel+1, len(hits)-1)
	}
	if imgui.IsKeyPressedBool(imgui.KeyUpArrow) {
		p.sel = max(p.sel-1, 0)
	}
	choose := -1
	if imgui.IsKeyPressedBool(imgui.KeyEnter) || imgui.IsKeyPressedBool(imgui.KeyKeypadEnter) {
		choose = p.sel
	}
	rowH := imgui.FrameHeight() + a.px(4)
	listH := min(float32(len(hits)), 9)*(rowH+2*imgui.CurrentStyle().ItemSpacing().Y) + a.px(4)
	imgui.BeginChildStrV("##plist", v2(0, max(listH, rowH)), 0, 0)
	if len(hits) == 0 {
		textC(cMuted, "No matching action.")
	}
	for i := range hits {
		it := &hits[i]
		act := it.Action
		sel := i == p.sel
		start := imgui.CursorPos()
		if imgui.SelectableBoolV("##pi"+strconv.Itoa(i), sel, 0, v2(0, rowH)) {
			choose = i
		}
		if sel && (imgui.IsKeyPressedBool(imgui.KeyDownArrow) || imgui.IsKeyPressedBool(imgui.KeyUpArrow)) {
			imgui.SetScrollHereY()
		}
		end := imgui.CursorPos()
		imgui.SetCursorPos(v2(start.X+a.px(8), start.Y+(rowH-imgui.TextLineHeight())/2))
		gc := cPrimary
		if act.Danger != actions.None || act.Group == actions.GroupDanger {
			gc = cError
		}
		textC(gc, Glyph(act.Icon))
		imgui.SameLineV(start.X+a.px(36), 0)
		text(act.Label)
		imgui.SameLineV(0, a.px(10))
		textC(cAccent, it.Context)
		imgui.SameLine()
		rightAlign(imgui.CalcTextSize(act.Group).X + a.px(8))
		textC(cDim, act.Group)
		imgui.SetCursorPos(end)
		imgui.Dummy(v2(0, 0))
	}
	imgui.EndChild()
	textC(cDim, "↑↓ navigate · Enter run · Esc close")
	close := imgui.IsKeyPressedBool(imgui.KeyEscape)
	if choose >= 0 && choose < len(hits) {
		it := hits[choose]
		close = true
		defer a.request(it.Action, it.Target, it.Context, true, nil)
	}
	if close {
		imgui.CloseCurrentPopup()
		p.open = false
	}
	imgui.EndPopup()
	imgui.PopStyleVar()
}

// ── copy-the-command dialog (no terminal emulator found) ─────────────────────

const copyID = "##copy-cmd"

func (a *app) copyWindow() {
	if a.ui.copyOpen {
		imgui.OpenPopupStr(copyID)
		a.ui.copyOpen = false
	}
	vp := imgui.MainViewport()
	imgui.SetNextWindowPosV(v2(vp.Pos().X+vp.Size().X/2, vp.Pos().Y+vp.Size().Y*0.4), imgui.CondAppearing, v2(0.5, 0.5))
	imgui.SetNextWindowSizeV(v2(a.px(600), 0), imgui.CondAlways)
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(20), a.px(16)))
	if imgui.BeginPopupModalV(copyID, nil, imgui.WindowFlagsNoTitleBar|imgui.WindowFlagsAlwaysAutoResize) {
		textC(cWarning, Glyph("terminal"))
		imgui.SameLineV(0, a.px(10))
		heading("Run this in a terminal", 18)
		textWrapped(cMuted, "This action is interactive and no terminal emulator was found ($TERMINAL, x-terminal-emulator, kitty, gnome-terminal, konsole, alacritty, xterm).")
		cmd := a.ui.copyCmd
		imgui.SetNextItemWidth(-1)
		withFont(fontMono, 0, func() {
			imgui.InputTextWithHint("##cmdcopy", "", &cmd, imgui.InputTextFlagsReadOnly|imgui.InputTextFlagsAutoSelectAll, nil)
		})
		if button(glyphCopy+"  Copy##cp", tonePrimary, false) {
			imgui.SetClipboardText(a.ui.copyCmd)
			a.notify("Command copied", toastInfo)
			imgui.CloseCurrentPopup()
		}
		imgui.SameLine()
		if button("Close##cpclose", toneGhost, false) || imgui.IsKeyPressedBool(imgui.KeyEscape) {
			imgui.CloseCurrentPopup()
		}
		imgui.EndPopup()
	}
	imgui.PopStyleVar()
}

// ── toasts ────────────────────────────────────────────────────────────────────

func (a *app) toastsWindow() {
	if len(a.v.toasts) == 0 {
		return
	}
	vp := imgui.MainViewport()
	bottom := vp.Pos().Y + vp.Size().Y - imgui.FrameHeight() - a.px(24)
	if a.ui.consoleOpen {
		bottom -= max(a.px(250), vp.Size().Y*0.3)
	}
	imgui.SetNextWindowPosV(v2(vp.Pos().X+vp.Size().X-a.px(18), bottom), imgui.CondAlways, v2(1, 1))
	imgui.SetNextWindowBgAlpha(0)
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(0, 0))
	imgui.BeginV("##toasts", nil, imgui.WindowFlagsNoDecoration|imgui.WindowFlagsAlwaysAutoResize|imgui.WindowFlagsNoSavedSettings|
		imgui.WindowFlagsNoFocusOnAppearing|imgui.WindowFlagsNoNav|imgui.WindowFlagsNoInputs|imgui.WindowFlagsNoMove)
	for i, t := range a.v.toasts {
		c, g := cPrimary, Glyph("info")
		switch t.Kind {
		case toastOK:
			c, g = cSuccess, glyphOK
		case toastFail:
			c, g = cError, glyphFail
		}
		msg := t.Text
		if len(msg) > 90 {
			msg = msg[:87] + "…"
		}
		imgui.PushStyleColorVec4(imgui.ColChildBg, vec4(cRaised))
		imgui.PushStyleColorVec4(imgui.ColBorder, vec4(c.alpha(0.7)))
		imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(14), a.px(10)))
		w := imgui.CalcTextSize(msg).X + a.px(60)
		imgui.BeginChildStrV("##toast"+strconv.Itoa(i), v2(min(w, a.px(560)), imgui.FrameHeight()+a.px(14)), imgui.ChildFlagsBorders|imgui.ChildFlagsAlwaysUseWindowPadding, imgui.WindowFlagsNoScrollbar)
		textC(c, g)
		imgui.SameLineV(0, a.px(10))
		text(msg)
		imgui.EndChild()
		imgui.PopStyleVar()
		imgui.PopStyleColorV(2)
		imgui.Dummy(v2(0, a.px(4)))
	}
	imgui.End()
	imgui.PopStyleVar()
}

// ── console ───────────────────────────────────────────────────────────────────

func (a *app) consoleJobView() (job, bool) {
	id := a.ui.consoleJob
	for i := len(a.v.jobs) - 1; i >= 0; i-- {
		if a.v.jobs[i].ID == id || id == 0 {
			return a.v.jobs[i], true
		}
	}
	if n := len(a.v.jobs); n > 0 {
		return a.v.jobs[n-1], true
	}
	return job{}, false
}

func (a *app) jobHeader(j job) {
	switch {
	case j.Running:
		spinner(a.px(6), cPrimary)
	case j.ok():
		textC(cSuccess, glyphOK)
	default:
		textC(cError, glyphFail)
	}
	imgui.SameLineV(0, a.px(8))
	withFont(fontBold, 0, func() { text(j.Label) })
	imgui.SameLineV(0, a.px(10))
	switch {
	case j.Running:
		textC(cMuted, "running "+time.Since(j.Started).Round(time.Second).String())
	case j.ok():
		textC(cSuccess, "done in "+j.Ended.Sub(j.Started).Round(100*time.Millisecond).String())
	default:
		textC(cError, fmt.Sprintf("exit %d", j.Code))
	}
}

func (a *app) console(h float32) {
	imgui.PushStyleColorVec4(imgui.ColChildBg, vec4(cPanel))
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(14), a.px(6)))
	imgui.PushStyleVarVec2(imgui.StyleVarItemSpacing, v2(a.px(8), a.px(6)))
	imgui.BeginChildStrV("##console", v2(0, h), imgui.ChildFlagsAlwaysUseWindowPadding, imgui.WindowFlagsNoScrollbar)
	dl := imgui.WindowDrawList()
	p := imgui.CursorScreenPos()
	dl.AddLine(v2(p.X-a.px(14), p.Y-a.px(6)), v2(p.X+imgui.WindowWidth(), p.Y-a.px(6)), u32(cBorder))

	chev := glyphChevronUp
	if a.ui.consoleOpen {
		chev = glyphChevronDn
	}
	if button(chev+"  "+glyphCommand+"  Output##ctoggle", toneGhost, false) {
		a.ui.consoleOpen = !a.ui.consoleOpen
	}
	tip("Show / hide command output (Ctrl+J)")
	j, has := a.consoleJobView()
	if has {
		imgui.SameLine()
		imgui.SetNextItemWidth(a.px(320))
		if imgui.BeginComboV("##jobsel", j.Label, 0) {
			for i := len(a.v.jobs) - 1; i >= 0; i-- {
				x := a.v.jobs[i]
				mark := glyphOK
				if x.Running {
					mark = Glyph("play")
				} else if !x.ok() {
					mark = glyphFail
				}
				if imgui.SelectableBoolV(mark+"  "+x.Label+"##job"+strconv.Itoa(x.ID), x.ID == j.ID, 0, v2(0, 0)) {
					a.ui.consoleJob = x.ID
					a.ui.consoleOpen = true
				}
			}
			imgui.EndCombo()
		}
		imgui.SameLine()
		a.jobHeader(j)
		// Right-side controls.
		ctrl := buttonWidth(glyphCopy) + buttonWidth(glyphClear) + buttonWidth(Glyph("trash")) + 2*imgui.CurrentStyle().ItemSpacing().X
		if j.Running {
			ctrl += buttonWidth(Glyph("stop")+"  Stop") + imgui.CurrentStyle().ItemSpacing().X
		}
		imgui.SameLine()
		rightAlign(ctrl)
		if j.Running {
			if button(Glyph("stop")+"  Stop##cstop", toneDanger, false) {
				a.stop(j.ID)
			}
			imgui.SameLine()
		}
		if button(glyphCopy+"##ccopy", toneGhost, false) {
			imgui.SetClipboardText(strings.Join(j.Lines, "\n"))
			a.notify("Output copied", toastInfo)
		}
		tip("Copy output")
		imgui.SameLine()
		if button(glyphClear+"##cclear", toneGhost, false) {
			a.clearJob(j.ID)
		}
		tip("Clear this output")
		imgui.SameLine()
		if button(Glyph("trash")+"##cfin", toneGhost, false) {
			a.clearFinished()
			a.ui.consoleJob = 0
		}
		tip("Forget finished commands")
	} else {
		imgui.SameLine()
		imgui.AlignTextToFramePadding()
		textC(cDim, "No commands run yet.")
	}
	if a.ui.consoleOpen && has {
		withFont(fontMono, 0, func() { textC(cDim, "$ homelab "+shellQuote(j.Argv)) })
		lines := j.Lines
		if j.Err != "" && !j.Running {
			lines = append(append([]string(nil), lines...), "error: "+j.Err)
		}
		logLines("##consolelog", lines, 0)
	}
	imgui.EndChild()
	imgui.PopStyleVarV(2)
	imgui.PopStyleColor()
}

// ── status bar ────────────────────────────────────────────────────────────────

func (a *app) statusBar(h float32) {
	imgui.PushStyleColorVec4(imgui.ColChildBg, vec4(cBgDark))
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(a.px(14), (h-imgui.TextLineHeight())/2))
	imgui.PushStyleVarVec2(imgui.StyleVarItemSpacing, v2(a.px(8), 0))
	imgui.BeginChildStrV("##status", v2(0, h), imgui.ChildFlagsAlwaysUseWindowPadding, imgui.WindowFlagsNoScrollbar)
	var running []job
	for i := range a.v.jobs {
		j := &a.v.jobs[i]
		if j.Running && !j.Stream {
			running = append(running, *j)
		}
	}
	streams := 0
	for i := range a.v.jobs {
		j := &a.v.jobs[i]
		if j.Running && j.Stream {
			streams++
		}
	}
	switch {
	case len(running) > 0:
		spinner(a.px(6), cPrimary)
		imgui.SameLine()
		lbl := running[0].Label + "…"
		if len(running) > 1 {
			lbl += fmt.Sprintf("  (+%d)", len(running)-1)
		}
		text(lbl)
	case !a.v.loaded:
		spinner(a.px(6), cPrimary)
		imgui.SameLine()
		textC(cMuted, "Loading services…")
	default:
		textC(cSuccess, glyphOK)
		imgui.SameLine()
		textC(cMuted, "Ready")
	}
	if streams > 0 {
		imgui.SameLineV(0, a.px(18))
		textC(cAccent, Glyph("logs"))
		imgui.SameLine()
		textC(cMuted, fmt.Sprintf("%d stream(s)", streams))
	}

	running2, installed := 0, 0
	for i := range a.v.services {
		s := &a.v.services[i]
		if s.Installed {
			installed++
			if s.Running > 0 {
				running2++
			}
		}
	}
	right := fmt.Sprintf("%d/%d services up", running2, installed)
	if !a.v.lastRefresh.IsZero() {
		right += "  ·  refreshed " + ago(a.v.lastRefresh)
	}
	hint := "Ctrl+K  commands"
	imgui.SameLine()
	rightAlign(imgui.CalcTextSize(right).X + imgui.CalcTextSize(hint).X + a.px(28))
	textC(cDim, right)
	imgui.SameLineV(0, a.px(28))
	textC(cMuted, hint)
	if imgui.IsItemClicked() {
		a.openPalette()
	}
	imgui.EndChild()
	imgui.PopStyleVarV(2)
	imgui.PopStyleColor()
}
