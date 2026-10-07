package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// View layer: every function here turns Model state into a string and touches
// nothing else — no Docker, no filesystem, no config.

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}
	if m.mode == modeLogs {
		return clampFrame(m.logv.View(), m.width, m.height)
	}
	return clampFrame(lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		m.renderTabs(),
		m.renderBody(),
		m.renderStatusBar(),
	), m.width, m.height)
}

// clampFrame makes s exactly h lines of at most w cells: the frame must
// never grow past the terminal, or the header scrolls off the top.
func clampFrame(s string, w, h int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = clipWidth(l, w)
	}
	return strings.Join(lines, "\n")
}

// ── Header ────────────────────────────────────────────────────────────────────

func (m Model) renderHeader() string {
	pills := []string{m.corePill(styles.Icon("box"), caddyContainer, m.core[caddyContainer])}
	for _, l := range m.offeredLayers() {
		pills = append(pills, m.corePill(styles.Icon(layerIcon(l.Name())), l.Name(), m.core[l.ContainerName()]))
	}
	right := strings.Join(pills, " ")

	left := styles.Header.Render(styles.Icon("home") + " homelab")
	if s := m.serviceCountSummary(); s != "" {
		left += "  " + s
	}
	if lipgloss.Width(left)+lipgloss.Width(right)+3 > m.width {
		right = "" // narrow: the counts matter more; Network shows the pills
	}
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right)-2, 1)
	return styles.Bar.Width(m.width).Padding(0, 1).Render(clipWidth(left+strings.Repeat(" ", gap)+right, m.width-2))
}

// corePill is one core container: its icon and name in its state's colour.
func (m Model) corePill(icon, name, state string) string {
	col := styles.ColStopped
	switch state {
	case containerStateRunning:
		col = styles.ColRunning
	case "":
	default:
		col = styles.ColPartial
	}
	return lipgloss.NewStyle().Foreground(col).Render(icon + " " + name)
}

func (m Model) serviceCountSummary() string {
	var running, installed, available int
	for i := range m.services {
		s := &m.services[i]
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
		parts = append(parts, styles.Subtle.Render(fmt.Sprintf("%d installed", installed)))
	}
	if available > 0 {
		parts = append(parts, styles.Muted.Render(fmt.Sprintf("%d available", available)))
	}
	return strings.Join(parts, styles.Muted.Render(" · "))
}

// ── Tabs ──────────────────────────────────────────────────────────────────────

func (m Model) tabLabel(v view) string {
	label := fmt.Sprintf("%d %s %s", v+1, styles.Icon(viewIcons[v]), viewNames[v])
	if m.width < 70 {
		label = fmt.Sprintf("%d %s", v+1, styles.Icon(viewIcons[v]))
		if v == m.view {
			label += " " + viewNames[v]
		}
	}
	if v == m.view {
		return styles.TabActive.Render(label)
	}
	return styles.Tab.Render(label)
}

func (m Model) renderTabs() string {
	var parts []string
	for v := view(0); v < numViews; v++ {
		parts = append(parts, m.tabLabel(v))
	}
	return " " + strings.Join(parts, "")
}

// tabRanges are the [start, end) columns of each tab, for mouse clicks.
func (m Model) tabRanges() [][2]int {
	var out [][2]int
	x := 1
	for v := view(0); v < numViews; v++ {
		w := lipgloss.Width(m.tabLabel(v))
		out = append(out, [2]int{x, x + w})
		x += w
	}
	return out
}

// ── Body ──────────────────────────────────────────────────────────────────────

// listWidth is the left column; narrow terminals show the list alone.
func (m Model) listWidth() int {
	if m.width < 70 {
		return m.width
	}
	return min(42, m.width*2/5)
}

func (m Model) renderBody() string {
	h := m.bodyHeight()
	if m.mode != modeNormal && m.mode != modeFilter {
		return m.renderModal(h)
	}
	lw := m.listWidth()
	list := fitBlock(m.renderList(h, lw), lw, h)
	if lw >= m.width {
		return list
	}
	rw := m.width - lw - 1
	sep := styles.PaneBorder.Render(strings.TrimRight(strings.Repeat("│\n", h), "\n"))
	return lipgloss.JoinHorizontal(lipgloss.Top, list, sep, fitBlock(m.renderDetail(h, rw), rw, h))
}

// fitBlock clamps s to exactly h lines of exactly w cells.
func fitBlock(s string, w, h int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).Render(strings.Join(lines, "\n"))
}

// ── List pane ─────────────────────────────────────────────────────────────────

// listOffset is the first row shown, keeping the cursor visible.
func (m Model) listOffset() int {
	rows := max(m.bodyHeight()-1, 1)
	c, n := m.cursor[m.view], m.rowCount(m.view)
	if c < rows {
		return 0
	}
	return max(min(c-rows/3, n-rows), 0)
}

func (m Model) renderList(h, w int) string {
	var b strings.Builder
	b.WriteString(m.renderListTitle() + "\n")
	rows := max(h-1, 1)
	off, n := m.listOffset(), m.rowCount(m.view)
	if n == 0 {
		b.WriteString(" " + styles.Muted.Render(m.emptyText()) + "\n")
	}
	for i := off; i < n && i < off+rows; i++ {
		b.WriteString(clipWidth(m.renderRow(i, i == m.cursor[m.view], w), w) + "\n")
	}
	return b.String()
}

func (m Model) emptyText() string {
	switch m.view {
	case viewBackups:
		if m.backErr != "" {
			return m.backErr
		}
		return "No backups yet — press b to make one"
	case viewCatalog:
		return "Nothing matches"
	}
	return "Nothing here"
}

func (m Model) renderListTitle() string {
	if m.mode == modeFilter {
		return " " + styles.Warning.Render(styles.Icon("search")+" ") + styles.Text.Render(m.filter[m.view]) + styles.Primary.Render("▏")
	}
	title := " " + styles.PaneTitle.Render(viewNames[m.view])
	detail := fmt.Sprintf("%d", m.rowCount(m.view))
	if f := m.filter[m.view]; f != "" {
		detail = styles.Icon("search") + " " + f + " · " + detail
	}
	if marks := m.markedNames(); len(marks) > 0 && m.view == viewServices {
		detail += " · " + styles.Accent.Render(fmt.Sprintf("%d marked", len(marks)))
	}
	return title + "  " + styles.Muted.Render(detail)
}

func cursorMark(selected bool) string {
	if selected {
		return styles.Primary.Render(styles.Icon("cursor") + " ")
	}
	return "  "
}

func nameStyle(selected bool) lipgloss.Style {
	if selected {
		return lipgloss.NewStyle().Bold(true).Foreground(styles.ColText)
	}
	return styles.Subtle
}

func (m Model) renderRow(i int, sel bool, w int) string {
	switch m.view {
	case viewServices:
		return m.renderServiceRow(m.visibleServices()[i], sel, w)
	case viewCatalog:
		s := m.visibleCatalog()[i]
		return cursorMark(sel) + styles.Muted.Render(styles.Icon("catalog")) + " " + nameStyle(sel).Render(clip(s.Name, w-4))
	case viewNetwork:
		if i == 0 {
			c := m.coreService()
			return cursorMark(sel) + styles.StateGlyph(c.Running, c.Total) + " " +
				nameStyle(sel).Width(max(w-12, 4)).Render("core stack") + styles.Muted.Render(fmt.Sprintf("%d/%d", c.Running, c.Total))
		}
		l := m.extensions()[i-1]
		state := styles.Muted.Render("off")
		running := 0
		if m.layerRunning(l) {
			running = 1
		}
		if m.enabled[l.Name()] {
			state = lipgloss.NewStyle().Foreground(styles.StateColor(running, 1)).Render(map[bool]string{true: "running", false: "stopped"}[running == 1])
		}
		return cursorMark(sel) + lipgloss.NewStyle().Foreground(styles.StateColor(running, 1)).Render(styles.Icon(layerIcon(l.Name()))) + " " +
			nameStyle(sel).Width(max(w-14, 4)).Render(clip(l.Name(), max(w-14, 4))) + state
	case viewBackups:
		bk := m.backups[i]
		live := ""
		if bk.Live {
			live = styles.Warning.Render(" live")
		}
		return cursorMark(sel) + styles.Blue.Render(styles.Icon("backup")) + " " +
			nameStyle(sel).Render(bk.Created.Local().Format("2006-01-02 15:04")) + " " +
			styles.Muted.Render(fmt.Sprintf("%d svc", len(bk.Services))) + live
	case viewHealth:
		r := m.healthRows()[i]
		mark := " "
		if res, ok := m.health[r.a.ID]; ok {
			mark = styles.Success.Render(styles.Icon("ok"))
			if res.err != nil {
				mark = styles.Danger.Render(styles.Icon("error"))
			}
		}
		scope := styles.Muted.Render(" " + r.t.Scope.String())
		return cursorMark(sel) + mark + " " + styles.Primary.Render(styles.Icon(r.a.Icon)) + " " +
			nameStyle(sel).Render(clip(r.a.Label, max(w-16, 4))) + scope
	}
	return ""
}

func (m Model) renderServiceRow(svc service.Service, sel bool, w int) string {
	if isCore(&svc) {
		return cursorMark(sel) + "  " + styles.StateGlyph(svc.Running, svc.Total) + " " +
			lipgloss.NewStyle().Bold(true).Foreground(styles.ColAccent).Width(max(w-14, 4)).Render(styles.Icon("core")+" core") +
			styles.Muted.Render(fmt.Sprintf("%d/%d", svc.Running, svc.Total))
	}
	mark := "  "
	if m.marked[svc.Name] {
		mark = styles.Accent.Render(styles.Icon("marked")) + " "
	}
	badges := m.layerBadges(svc)
	nameW := max(w-6-lipgloss.Width(badges)-2, 4)
	return cursorMark(sel) + mark + styles.StateGlyph(svc.Running, svc.Total) + " " +
		nameStyle(sel).Width(nameW).Render(clip(svc.Name, nameW)) + " " + badges
}

// layerBadges is one icon per layer the service is exposed on.
func (m Model) layerBadges(svc service.Service) string {
	var b strings.Builder
	for _, l := range m.opt.Layers {
		if svc.On(l.Name()) {
			b.WriteString(styles.Exposed.Render(styles.Icon(layerIcon(l.Name()))))
		}
	}
	return b.String()
}

// ── Detail pane ───────────────────────────────────────────────────────────────

func (m Model) renderDetail(h, w int) string {
	var lines []string
	switch m.view {
	case viewServices, viewCatalog:
		svc := m.selectedService()
		switch {
		case len(m.markedNames()) > 0 && m.view == viewServices:
			lines = m.multiDetail(w)
		case svc == nil:
			lines = []string{styles.Muted.Render("No service selected")}
		case isCore(svc):
			lines = m.coreDetail(w)
		case !svc.Installed:
			lines = m.catalogDetail(svc, w)
		default:
			lines = m.serviceDetail(svc, w)
		}
	case viewNetwork:
		if l := m.selectedLayer(); l != nil {
			lines = m.layerDetail(w)
		} else {
			lines = m.coreDetail(w)
		}
	case viewBackups:
		lines = m.backupDetail(w)
	case viewHealth:
		return m.healthDetail(h, w)
	}
	if t, ok := m.target(); ok && m.view != viewBackups {
		lines = append(lines, m.actionLines(t, h-len(lines), w)...)
	}
	if m.view == viewServices && len(m.markedNames()) == 0 {
		lines = append(lines, m.logTail(m.selectedService(), w)...)
	}
	return indent(lines, h, w)
}

// indent pads lines into a w-wide, h-high block with a one-cell margin.
func indent(lines []string, h, w int) string {
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		lines[i] = " " + clipWidth(l, w-1)
	}
	return strings.Join(lines, "\n")
}

func titleLine(icon, name, tag string, w int) []string {
	return []string{
		styles.Bold.Render(icon+" "+name) + "  " + tag,
		styles.PaneBorder.Render(strings.Repeat("─", max(w-2, 1))),
	}
}

func stateTag(running, total int) string {
	st := lipgloss.NewStyle().Foreground(styles.StateColor(running, total))
	if total == 0 {
		return st.Render(styles.Icon("stopped") + " stopped")
	}
	return st.Render(fmt.Sprintf("%s %d/%d running", styles.Icon(map[bool]string{true: "running", false: "partial"}[running == total]), running, total))
}

func section(title string) string { return styles.GroupTitle.Render(title) }

func (m Model) serviceDetail(svc *service.Service, w int) []string {
	lines := titleLine(styles.Icon("box"), svc.Name, stateTag(svc.Running, svc.Total), w)

	env := m.rootEnv()
	lines = append(lines, "", section("Access"))
	for _, l := range m.offeredLayers() {
		icon := styles.Icon(layerIcon(l.Name()))
		tag := fmt.Sprintf("%-4s", l.Name())
		if !svc.On(l.Name()) {
			lines = append(lines, styles.Muted.Render(icon+" "+tag+" off"))
			continue
		}
		for _, addr := range l.ServiceAddresses(svc.Name, env) {
			text := styles.Primary.Render(addr.URL)
			if addr.Note != "" {
				text += " " + styles.Muted.Render("("+addr.Note+")")
			}
			lines = append(lines, styles.Exposed.Render(icon)+" "+tag+" "+text)
		}
	}

	if len(svc.Containers) > 0 {
		lines = append(lines, "", section("Containers"))
		for i := range svc.Containers {
			c := &svc.Containers[i]
			extra := ""
			if i < len(m.containerDetails) {
				d := m.containerDetails[i]
				if d.Health != "" {
					extra += "  " + styles.HealthTag(d.Health)
				}
				if len(d.Ports) > 0 {
					extra += "  " + styles.Muted.Render(clip(strings.Join(d.Ports, ", "), 28))
				}
			}
			lines = append(lines, lipgloss.NewStyle().Width(22).Render(clip(c.Name, 21))+styles.StateTag(c.State)+extra)
		}
	}

	return lines
}

// logTail is the selected service's last log lines, shown under its actions.
func (m Model) logTail(svc *service.Service, w int) []string {
	if len(m.logLines) == 0 || svc == nil || m.logSvcName != svc.Name {
		return nil
	}
	lines := []string{"", section("Logs")}
	for _, l := range m.logLines {
		lines = append(lines, styles.Muted.Render(clip(stripAnsi(l), w-3)))
	}
	return lines
}

func (m Model) coreDetail(w int) []string {
	c := m.coreService()
	lines := titleLine(styles.Icon("core"), "core stack", stateTag(c.Running, c.Total), w)
	lines = append(lines, "", section("Containers"))
	for _, name := range m.coreHeaderContainers() {
		st := m.core[name]
		if st == "" {
			st = "not running"
		}
		lines = append(lines, lipgloss.NewStyle().Width(14).Render(name)+styles.StateTag(st))
	}
	return lines
}

func (m Model) catalogDetail(svc *service.Service, w int) []string {
	lines := titleLine(styles.Icon("catalog"), svc.Name, styles.Muted.Render("not installed"), w)
	return append(lines, "",
		styles.Subtle.Render("Bundled with homelab. Install it, then:"),
		styles.Muted.Render("  configure "+styles.Icon("gear")+"  ·  up "+styles.Icon("play")+"  ·  expose "+styles.Icon("shield")),
		"", styles.Key(keyEnter)+" install")
}

func (m Model) multiDetail(w int) []string {
	t := m.multiTarget()
	lines := titleLine(styles.Icon("marked"), fmt.Sprintf("%d services", len(t.Names)), stateTag(t.Running, t.Total), w)
	lines = append(lines, styles.Accent.Render(clip(strings.Join(t.Names, ", "), w*2)), "",
		styles.Muted.Render("Actions run on every marked service. esc clears."))
	return lines
}

func (m Model) layerDetail(w int) []string {
	l := m.selectedLayer()
	running := 0
	if m.layerRunning(l) {
		running = 1
	}
	tag := styles.Muted.Render("disabled")
	if m.enabled[l.Name()] {
		tag = stateTag(running, 1)
	}
	lines := titleLine(styles.Icon(layerIcon(l.Name())), l.Name(), tag, w)
	exposed := 0
	for i := range m.services {
		s := &m.services[i]
		if s.On(l.Name()) {
			exposed++
		}
	}
	return append(lines, styles.Subtle.Render(l.Label()),
		styles.Muted.Render(fmt.Sprintf("container %s · %d services exposed", l.ContainerName(), exposed)))
}

func (m Model) backupDetail(w int) []string {
	b := m.selectedBackup()
	if b == nil {
		return []string{styles.Muted.Render("Backups live in <config-dir>/backups."), "",
			styles.Key("b") + " back up every service"}
	}
	lines := titleLine(styles.Icon("backup"), b.Created.Local().Format("2006-01-02 15:04:05"), "", w)
	lines = append(lines, styles.Muted.Render(clip(b.Dir, w*2)), "", section("Services"))
	for _, s := range b.Services {
		lines = append(lines, "  "+s)
	}
	if b.Live {
		lines = append(lines, "", styles.Warning.Render(styles.Icon("warn")+" taken live — files may be torn"))
	}
	return append(lines, "", styles.Key("r")+" restore  "+styles.Key("b")+" new backup  "+styles.Key(keyEnter)+" all actions")
}

func (m Model) healthDetail(h, w int) string {
	r := m.selectedHealth()
	if r == nil {
		return ""
	}
	lines := titleLine(styles.Icon(r.a.Icon), r.a.Label, styles.Muted.Render(r.t.Scope.String()), w)
	if r.a.Help != "" {
		lines = append(lines, styles.Muted.Render(r.a.Help))
	}
	res, ok := m.health[r.a.ID]
	if !ok {
		lines = append(lines, "", styles.Key(keyEnter)+" run   "+styles.Key(":")+" with options")
		return indent(lines, h, w)
	}
	status := styles.Success.Render(styles.Icon("ok") + " passed")
	if res.err != nil {
		status = styles.Danger.Render(styles.Icon("error") + " " + res.err.Error())
	}
	lines = append(lines, status+"  "+styles.Muted.Render("J/K scroll · enter re-run"), "")
	out := strings.Split(strings.TrimRight(stripAnsi(res.out), "\n"), "\n")
	top := min(m.healthScroll, max(len(out)-1, 0))
	lines = append(lines, out[top:]...)
	return indent(lines, h, w)
}

// actionLines lists what can be done to t, with shortcut keys, in the space
// left: the registry, not a hand-kept list, decides what shows.
func (m Model) actionLines(t actions.Target, room, w int) []string {
	acts := actions.For(t)
	if m.view == viewServices && len(m.logLines) > 0 {
		room = min(room, 8) // leave the log tail some room
	}
	if room < 4 || len(acts) == 0 {
		return nil
	}
	lines := []string{"", section("Actions") + "  " + styles.Muted.Render("enter for all")}
	room -= 2
	// Shortcut actions first: they are the ones a key reaches.
	var keyed, rest []actions.Action
	for i := range acts {
		a := &acts[i]
		if keyFor(a.ID) != "" {
			keyed = append(keyed, *a)
		} else {
			rest = append(rest, *a)
		}
	}
	var cells []string
	keyed = append(keyed, rest...)
	for i := range keyed {
		a := &keyed[i]
		k := keyFor(a.ID)
		if k == "" {
			k = " "
		}
		label := a.Label
		st := styles.Text
		if a.Danger != actions.None {
			st = styles.Danger
		}
		cells = append(cells, styles.Key(k)+" "+st.Render(styles.Icon(a.Icon)+" "+label))
	}
	cols := 1
	if w >= 60 {
		cols = 2
	}
	colW := (w - 2) / cols
	for i := 0; i < len(cells) && room > 0; i += cols {
		var row strings.Builder
		for c := 0; c < cols && i+c < len(cells); c++ {
			row.WriteString(lipgloss.NewStyle().Width(colW).MaxWidth(colW).Render(clipWidth(cells[i+c], colW-1)))
		}
		lines = append(lines, row.String())
		room--
	}
	return lines
}

// ── Status bar ────────────────────────────────────────────────────────────────

func (m Model) renderStatusBar() string {
	var s string
	switch {
	case m.running > 0 && m.mode == modeNormal:
		s = styles.Primary.Render(m.spin.View() + " " + m.busyMsg)
	case m.toast != "" && m.mode == modeNormal:
		if m.toastErr {
			s = styles.Danger.Render(styles.Icon("error") + " " + m.toast)
		} else {
			s = styles.Success.Render(m.toast)
		}
		if m.lastOut != nil {
			s += "  " + hintBar("o", "output")
		}
	default:
		s = m.hints()
	}
	return styles.Bar.Width(m.width).MaxHeight(1).Padding(0, 1).Render(clipWidth(s, m.width-2))
}

func (m Model) hints() string {
	switch m.mode {
	case modeFilter:
		return hintBar(keyEnter, "keep", "esc", "clear")
	case modePalette:
		return hintBar("type", "filter", "↑↓", "move", keyEnter, "run", "esc", "close")
	case modeForm:
		return hintBar("tab", "next", "space/←→", "toggle·choose", keyEnter, "run", "esc", "cancel")
	case modeConfirm:
		return hintBar("y", "confirm", "n/esc", "cancel")
	case modeTyped:
		return hintBar(keyEnter, "confirm", "esc", "cancel")
	case modeOutput:
		return hintBar("↑↓ pgup/pgdn", "scroll", "esc", "close")
	case modeHelp:
		return hintBar("j/k", "scroll", "any key", "close")
	case modeSetup:
		return hintBar("tab", "next", keyEnter, "save", "ctrl+o", "wizard", "esc", "cancel")
	}
	pairs := []string{keyEnter, "actions", ":", "palette"}
	if t, ok := m.target(); ok {
		for _, s := range shortcuts[t.Scope] {
			if a, ok := actions.ByID(s.id); ok && a.Can(t) {
				pairs = append(pairs, s.key, strings.ToLower(a.Label))
			}
			if len(pairs) >= 14 {
				break
			}
		}
	}
	switch m.view {
	case viewServices:
		pairs = append(pairs, "space", "mark", "e", "expose", "/", "filter")
	case viewCatalog:
		pairs = append(pairs, "/", "filter")
	}
	return hintBar(append(pairs, "?", "help", "q", "quit")...)
}

// hintBar renders key/label pairs as "[k] label  [k] label".
func hintBar(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, styles.Key(pairs[i])+" "+styles.Subtle.Render(pairs[i+1]))
	}
	return strings.Join(parts, "  ")
}

// clipWidth trims a styled string to w visible cells so it never wraps.
func clipWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// stripAnsi removes ANSI escape sequences from command output.
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
