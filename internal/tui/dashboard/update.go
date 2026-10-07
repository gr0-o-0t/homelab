package dashboard

import (
	"maps"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/tui/logs"
)

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// The embedded log viewer owns every message while it is open, except
	// what the dashboard itself must keep tracking.
	if m.mode == modeLogs {
		switch msg := msg.(type) {
		case logs.ClosedMsg:
			m.mode = modeNormal
			return m, m.refreshAll()
		case tea.KeyMsg:
			if msg.String() == "ctrl+c" {
				m.logv.Stop()
				return m, tea.Quit
			}
		case tea.WindowSizeMsg:
			m.width, m.height = msg.Width, msg.Height
		case refreshedMsg, coreStatusMsg, backupsMsg, actionDoneMsg, coreTickMsg, logTickMsg, inspectTickMsg, spinner.TickMsg:
			return m.update(msg)
		}
		lm, cmd := m.logv.Update(msg)
		m.logv = lm.(logs.Model)
		return m, cmd
	}
	return m.update(msg)
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.mode == modeOutput {
			m = m.sizeOutput()
		}
		if m.mode == modeLogs {
			lm, _ := m.logv.Update(msg)
			m.logv = lm.(logs.Model)
		}

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case refreshedMsg:
		m.services = msg.services
		if msg.enabled != nil {
			m.enabled = msg.enabled
		}
		m = m.clampCursors()
		cmds = append(cmds, m.fetchLogsCmd())

	case refreshErrMsg:
		m.toast, m.toastErr = msg.err.Error(), true

	case coreStatusMsg:
		m.core = msg

	case backupsMsg:
		m.backups, m.backErr = msg.list, ""
		if msg.err != nil {
			m.backErr = msg.err.Error()
		}
		m = m.clampCursors()

	case logTailMsg:
		if msg.svcName == m.selectedName() {
			m.logLines, m.logSvcName = msg.lines, msg.svcName
		}

	case containerDetailMsg:
		if msg.svcName == m.selectedName() {
			m.containerDetails = msg.details
		}

	case coreTickMsg:
		// Containers change state behind the dashboard's back (crashes,
		// other terminals), so the list refreshes on the same tick.
		cmds = append(cmds, m.coreRefreshCmd(), m.refreshCmd(), coreTickCmd())

	case logTickMsg:
		cmds = append(cmds, m.fetchLogsCmd(), logTickCmd())

	case inspectTickMsg:
		cmds = append(cmds, m.fetchInspectCmd(), inspectTickCmd())

	case spinner.TickMsg:
		if m.running > 0 {
			var c tea.Cmd
			m.spin, c = m.spin.Update(msg)
			cmds = append(cmds, c)
		}

	case choicesMsg:
		if m.mode == modeForm {
			m.frm = m.frm.setChoices(msg.key, msg.choices)
			if msg.err != nil {
				m.frm.err = msg.err.Error()
			}
		}

	case setupLoadedMsg:
		if m.mode == modeSetup && m.setup.svc == msg.svc {
			if msg.err != nil {
				m.setup.loading, m.setup.err = false, msg.err.Error()
				break
			}
			p := m.setup.p
			m.setup = newSetupForm(msg.svc, msg.info)
			m.setup.p = p
		}

	case actionDoneMsg:
		return m.actionDone(msg)

	case execDoneMsg:
		m.toast, m.toastErr = "✓ "+msg.label+" finished", false
		if msg.err != nil {
			m.toast, m.toastErr = msg.label+": "+msg.err.Error(), true
		}
		cmds = append(cmds, m.refreshAll())
	}
	return m, tea.Batch(cmds...)
}

func (m Model) refreshAll() tea.Cmd {
	return tea.Batch(m.refreshCmd(), m.coreRefreshCmd(), m.backupsCmd())
}

// actionDone records a captured run: the Health pane for checks run there,
// the output modal for reports and failures, a toast otherwise. The full
// output is always kept for `o`.
func (m Model) actionDone(msg actionDoneMsg) (tea.Model, tea.Cmd) {
	m.running = max(m.running-1, 0)
	if m.running == 0 {
		m.busyMsg = ""
	}
	res := msg.res
	m.lastOut = &res
	a := msg.p.action

	if res.err != nil {
		m.toast, m.toastErr = res.title+" failed: "+firstNonEmpty(lastLine(res.out), res.err.Error()), true
	} else {
		m.toast, m.toastErr = "✓ "+res.title, false
	}

	switch {
	case msg.p.origin == viewHealth:
		m.health = maps.Clone(m.health)
		m.health[a.ID] = res
		m.healthScroll = 0
	case m.mode == modeNormal && (res.err != nil || showsReport(a, msg.p.inputs)):
		m = m.openOutput(res)
	}
	return m, m.refreshAll()
}

// showsReport reports whether an action's output is the point of running it.
func showsReport(a actions.Action, in actions.Inputs) bool {
	return a.Group == actions.GroupInspect || strings.HasSuffix(a.ID, ".list") || in.Bool("dry-run")
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func (m Model) openOutput(res result) Model {
	m.out = outputState{res: res}
	m.mode = modeOutput
	return m.sizeOutput()
}

// sizeOutput fits the output viewport into the modal.
func (m Model) sizeOutput() Model {
	w, h := m.modalSize(100)
	vp := viewport.New(max(w-4, 10), max(h-4, 1))
	body := strings.TrimRight(stripAnsi(m.out.res.out), "\n")
	if body == "" {
		body = "(no output)"
	}
	if m.out.res.err != nil {
		body += "\n\n" + "exit: " + m.out.res.err.Error()
	}
	vp.SetContent(body)
	m.out.vp = vp
	return m
}

// ── starting actions ──────────────────────────────────────────────────────────

// isSetup reports whether an action is the setup wizard, which the TUI opens
// as its own form (the wizard stays one key away in it).
func isSetup(a actions.Action) bool { return a.ID == "service.setup" || a.ID == "global.setup" }

// start begins running a on t. ask opens the input form even when every
// input has a default (the palette always asks; shortcuts run with defaults).
func (m Model) start(a actions.Action, t actions.Target, prefill actions.Inputs, ask bool) (Model, tea.Cmd) {
	m.toast, m.toastErr = "", false
	if !a.Can(t) {
		m.toast, m.toastErr = a.Label+" is not available for "+targetTitle(t), true
		return m, nil
	}
	p := pending{action: a, target: t, inputs: prefill, origin: m.view}
	if isSetup(a) {
		return m.openSetup(p)
	}
	if len(a.Inputs) > 0 && (ask || hasRequired(a)) {
		m.frm = newForm(p)
		m.mode = modeForm
		var cmds []tea.Cmd
		for _, in := range a.Inputs {
			if in.Kind == actions.Choice && in.Source != "" {
				cmds = append(cmds, m.choicesCmd(in))
			}
		}
		return m, tea.Batch(cmds...)
	}
	return m.proceed(p)
}

// hasRequired: a required input is always shown, even prefilled — it is
// what the action is about (the backup to restore, the port to look up).
func hasRequired(a actions.Action) bool {
	return slices.ContainsFunc(a.Inputs, func(i actions.Input) bool { return i.Required })
}

// proceed asks for the confirmation the action's Danger demands, then runs.
func (m Model) proceed(p pending) (Model, tea.Cmd) {
	switch p.action.Danger {
	case actions.Confirm:
		m.confirm = confirmState{p: p}
		m.mode = modeConfirm
		return m, nil
	case actions.TypeName:
		ti := newTextInput("", p.action.Token(p.target))
		ti.Focus()
		m.confirm = confirmState{p: p, token: p.action.Token(p.target), ti: ti}
		m.mode = modeTyped
		return m, textinput.Blink
	}
	return m.execute(p)
}

// execute runs p the way its kind needs: suspended for interactive commands,
// in the log viewer for streams, captured for everything else.
func (m Model) execute(p pending) (Model, tea.Cmd) {
	m.mode = modeNormal
	args := p.action.Build(p.target, p.inputs)
	label := p.action.Label + " · " + targetTitle(p.target)
	switch {
	case p.action.Interactive:
		return m, m.interactiveCmd(label, args)
	case p.action.Stream:
		if len(m.opt.CLI) == 0 {
			m.toast, m.toastErr = errNoCLI.Error(), true
			return m, nil
		}
		m.logv = logs.NewEmbedded(label, m.argv(args))
		m.streams.add(m.logv.Stop)
		lm, _ := m.logv.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
		m.logv = lm.(logs.Model)
		m.mode = modeLogs
		return m, m.logv.Init()
	}
	m.running++
	m.busyMsg = "homelab " + strings.Join(args, " ")
	return m, tea.Batch(m.runCmd(p, args), m.spin.Tick)
}

func (m Model) openSetup(p pending) (Model, tea.Cmd) {
	svc := ""
	if p.action.Scope == actions.Service {
		svc = p.target.Name
		if svc == "" && len(p.target.Names) == 1 {
			svc = p.target.Names[0]
		}
	}
	m.setup = setupForm{p: p, svc: svc, loading: true}
	m.mode = modeSetup
	return m, m.setupLoadCmd(svc)
}

// runShortcut runs the action bound to key for the selection, falling back
// to the stack-wide binding. handled is false when the key binds nothing.
func (m Model) runShortcut(key string) (Model, tea.Cmd, bool) {
	t, ok := m.target()
	if ok {
		if a, found := shortcutFor(t.Scope, key); found {
			mm, cmd := m.start(a, t, m.prefill(), false)
			return mm, cmd, true
		}
	}
	if a, found := shortcutFor(actions.Global, key); found {
		mm, cmd := m.start(a, m.globalTarget(), m.prefill(), false)
		return mm, cmd, true
	}
	return m, nil, false
}

// ── keys ──────────────────────────────────────────────────────────────────────

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		m.StopStreams()
		return m, tea.Quit
	}
	switch m.mode {
	case modeFilter:
		return m.keyFilter(msg)
	case modePalette:
		return m.keyPalette(msg)
	case modeForm:
		return m.keyForm(msg)
	case modeConfirm:
		switch k {
		case "y", "Y", "enter":
			return m.execute(m.confirm.p)
		case "n", "N", "esc", "q":
			m.mode = modeNormal
		}
		return m, nil
	case modeTyped:
		return m.keyTyped(msg)
	case modeOutput:
		switch k {
		case "esc", "q", "o", "enter":
			m.mode = modeNormal
			return m, nil
		}
		var cmd tea.Cmd
		m.out.vp, cmd = m.out.vp.Update(msg)
		return m, cmd
	case modeHelp:
		switch k {
		case "j", "down":
			m.helpTop++
		case "k", "up":
			m.helpTop = max(m.helpTop-1, 0)
		default:
			m.mode = modeNormal
		}
		return m, nil
	case modeSetup:
		return m.keySetup(msg)
	}
	return m.keyNormal(k)
}

func (m Model) keyNormal(k string) (tea.Model, tea.Cmd) {
	m.toast = ""
	if k != "g" {
		m.lastKey = ""
	}
	n := m.rowCount(m.view)
	moved := func(c int) (tea.Model, tea.Cmd) {
		m.cursor[m.view] = max(min(c, n-1), 0)
		m.healthScroll = 0
		return m, tea.Batch(m.fetchLogsCmd(), m.fetchInspectCmd())
	}
	switch k {
	case "q":
		m.StopStreams()
		return m, tea.Quit
	case "?":
		m.mode, m.helpTop = modeHelp, 0
	case "tab", "right":
		return m.switchView((m.view + 1) % numViews)
	case "shift+tab", "left":
		return m.switchView((m.view + numViews - 1) % numViews)
	case "1", "2", "3", "4", "5":
		return m.switchView(view(k[0] - '1'))
	case "up", "k":
		return moved(m.cursor[m.view] - 1)
	case "down", "j":
		return moved(m.cursor[m.view] + 1)
	case "g":
		if m.lastKey == "g" {
			m.lastKey = ""
			return moved(0)
		}
		m.lastKey = "g"
	case "home":
		return moved(0)
	case "G", "end":
		return moved(n - 1)
	case "ctrl+u", "pgup":
		return moved(m.cursor[m.view] - m.halfPage())
	case "ctrl+d", "pgdown":
		return moved(m.cursor[m.view] + m.halfPage())
	case "J":
		m.healthScroll++
	case "K":
		m.healthScroll = max(m.healthScroll-1, 0)
	case "/":
		if m.view == viewServices || m.view == viewCatalog {
			m.mode = modeFilter
			m.filter[m.view] = ""
			m.cursor[m.view] = 0
		}
	case "esc":
		m.filter[m.view] = ""
		m.marked = map[string]bool{}
		m = m.clampCursors()
	case "R":
		return m, m.refreshAll()
	case "o":
		if m.lastOut != nil {
			m = m.openOutput(*m.lastOut)
		} else {
			m.toast = "no command output yet"
		}
	case ":", "ctrl+p":
		m = m.openPalette("")
	case " ":
		if svc := m.selectedService(); m.view == viewServices && svc != nil && !isCore(svc) {
			m.marked = m.withMark(svc.Name)
			if m.cursor[m.view] < n-1 {
				m.cursor[m.view]++
			}
		}
	case "A":
		if m.view == viewServices {
			m.marked = map[string]bool{}
			for _, s := range m.visibleServices() {
				if !isCore(&s) {
					m.marked[s.Name] = true
				}
			}
		}
	case "e":
		if t, ok := m.target(); ok && t.Scope == actions.Service {
			m = m.openPalette(actions.GroupExposure)
			if len(m.pal.items) == 0 {
				m.mode = modeNormal
				m.toast, m.toastErr = "no exposure changes available for "+targetTitle(t), true
			}
			return m, nil
		}
		return m.shortcutOrNothing(k)
	case "enter":
		return m.enter()
	default:
		return m.shortcutOrNothing(k)
	}
	return m, nil
}

func (m Model) shortcutOrNothing(k string) (tea.Model, tea.Cmd) {
	mm, cmd, _ := m.runShortcut(k)
	return mm, cmd
}

// enter runs a view's primary action: install a catalog entry, run a check;
// everywhere else it opens the actions of the selection.
func (m Model) enter() (tea.Model, tea.Cmd) {
	switch m.view {
	case viewCatalog:
		if t, ok := m.target(); ok {
			if a, ok := shortcutFor(actions.CatalogEntry, "i"); ok {
				return m.start(a, t, nil, false)
			}
		}
		return m, nil
	case viewHealth:
		if r := m.selectedHealth(); r != nil {
			return m.start(r.a, r.t, nil, false)
		}
		return m, nil
	}
	return m.openPalette(""), nil
}

func (m Model) switchView(v view) (tea.Model, tea.Cmd) {
	m.view = v
	m.healthScroll = 0
	m = m.clampCursors()
	var cmds []tea.Cmd
	if v == viewBackups {
		cmds = append(cmds, m.backupsCmd())
	}
	cmds = append(cmds, m.fetchLogsCmd(), m.fetchInspectCmd())
	return m, tea.Batch(cmds...)
}

func (m Model) keyFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.mode = modeNormal
	case "esc":
		m.mode = modeNormal
		m.filter[m.view] = ""
	case "backspace":
		if f := []rune(m.filter[m.view]); len(f) > 0 {
			m.filter[m.view] = string(f[:len(f)-1])
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			m.filter[m.view] += string(msg.Runes)
		}
	}
	m.cursor[m.view] = 0
	return m, m.fetchLogsCmd()
}

func (m Model) keyPalette(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.pal.filtered())
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
	case "up", "ctrl+k", "shift+tab":
		m.pal.cursor = max(m.pal.cursor-1, 0)
	case "down", "ctrl+j", "tab":
		m.pal.cursor = min(m.pal.cursor+1, max(n-1, 0))
	case "pgup":
		m.pal.cursor = max(m.pal.cursor-10, 0)
	case "pgdown":
		m.pal.cursor = min(m.pal.cursor+10, max(n-1, 0))
	case "enter":
		if it, ok := m.pal.selected(); ok {
			m.mode = modeNormal
			return m.start(it.a, it.t, m.pal.prefill, true)
		}
	case "backspace":
		if q := []rune(m.pal.query); len(q) > 0 {
			m.pal.query = string(q[:len(q)-1])
			m.pal.cursor = 0
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			m.pal.query += string(msg.Runes)
			m.pal.cursor = 0
		}
	}
	return m, nil
}

func (m Model) keyForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.frm
	if len(f.fields) == 0 {
		m.mode = modeNormal
		return m, nil
	}
	fd := &f.fields[f.focus]
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		return m, nil
	case "tab", "down":
		m.frm = m.frm.focusField(f.focus + 1)
		return m, nil
	case "shift+tab", "up":
		m.frm = m.frm.focusField(f.focus - 1)
		return m, nil
	case "enter", "ctrl+s":
		in, err := m.frm.values()
		if err != nil {
			m.frm.err = err.Error()
			return m, nil
		}
		p := m.frm.p
		p.inputs = in
		return m.proceed(p)
	}
	switch fd.in.Kind {
	case actions.Bool:
		if k := msg.String(); k == " " || k == "x" || k == "left" || k == "right" {
			fd.on = !fd.on
		}
	case actions.Choice:
		if len(fd.choices) > 0 {
			switch msg.String() {
			case "right", "l", " ":
				fd.ci = (fd.ci + 1) % len(fd.choices)
			case "left", "h":
				fd.ci = (fd.ci + len(fd.choices) - 1) % len(fd.choices)
			}
		}
	default:
		var cmd tea.Cmd
		fd.ti, cmd = fd.ti.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) keyTyped(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		return m, nil
	case "enter":
		if strings.TrimSpace(m.confirm.ti.Value()) != m.confirm.token {
			m.confirm.err = "type " + m.confirm.token + " exactly to confirm"
			return m, nil
		}
		return m.execute(m.confirm.p)
	}
	var cmd tea.Cmd
	m.confirm.ti, cmd = m.confirm.ti.Update(msg)
	m.confirm.err = ""
	return m, cmd
}

func (m Model) keySetup(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := &m.setup
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		return m, nil
	case "ctrl+o":
		// The interactive wizard: the terminal is handed over to it.
		m.mode = modeNormal
		p := s.p
		return m, m.interactiveCmd(p.action.Label+" · "+targetTitle(p.target), p.action.Build(p.target, nil))
	}
	if s.loading || len(s.fields) == 0 {
		return m, nil
	}
	switch msg.String() {
	case "tab", "down":
		m.setup = s.focusField(s.focus + 1)
		return m, nil
	case "shift+tab", "up":
		m.setup = s.focusField(s.focus - 1)
		return m, nil
	case "enter", "ctrl+s":
		sets, secrets := s.changes()
		if len(sets) == 0 && len(secrets) == 0 {
			m.mode = modeNormal
			m.toast = "nothing changed"
			return m, nil
		}
		m.mode = modeNormal
		m.running++
		m.busyMsg = "homelab " + strings.Join(setupSaveArgs(s.svc, sets, secrets), " ")
		return m, tea.Batch(m.setupSaveCmd(s.p, s.svc, sets, secrets), m.spin.Tick)
	}
	var cmd tea.Cmd
	s.fields[s.focus].ti, cmd = s.fields[s.focus].ti.Update(msg)
	return m, cmd
}

// ── mouse ─────────────────────────────────────────────────────────────────────

func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeNormal {
		if m.mode == modeOutput {
			var cmd tea.Cmd
			m.out.vp, cmd = m.out.vp.Update(msg)
			return m, cmd
		}
		return m, nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.keyNormal("up")
	case tea.MouseButtonWheelDown:
		return m.keyNormal("down")
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		if msg.Y == 1 {
			for v, r := range m.tabRanges() {
				if msg.X >= r[0] && msg.X < r[1] {
					return m.switchView(view(v))
				}
			}
			return m, nil
		}
		row := msg.Y - headerLines - 1 // the list's title line
		if row >= 0 && msg.X < m.listWidth() {
			i := m.listOffset() + row
			if i < m.rowCount(m.view) {
				if i == m.cursor[m.view] {
					return m.enter()
				}
				m.cursor[m.view] = i
				return m, tea.Batch(m.fetchLogsCmd(), m.fetchInspectCmd())
			}
		}
	}
	return m, nil
}
