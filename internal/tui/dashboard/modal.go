package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/tui/styles"
)

// Modals replace the body while they have the keyboard, centred in it.

// modalSize is the outer size of a modal at most maxW wide.
func (m Model) modalSize(maxW int) (int, int) {
	return max(min(maxW, m.width-2), 20), max(m.bodyHeight(), 3)
}

// box frames lines (already at most inner width) in a rounded border, cut to
// h lines including the border.
func box(lines []string, w, h int, danger bool) string {
	inner := max(w-4, 1)
	if len(lines) > h-2 {
		lines = lines[:max(h-2, 1)]
	}
	for i, l := range lines {
		lines[i] = clipWidth(l, inner)
	}
	st := styles.Modal
	if danger {
		st = styles.ModalDanger
	}
	return st.Width(w - 2).Render(strings.Join(lines, "\n"))
}

func (m Model) renderModal(h int) string {
	var content string
	switch m.mode {
	case modePalette:
		content = m.renderPalette(h)
	case modeForm:
		content = m.renderForm(h)
	case modeConfirm, modeTyped:
		content = m.renderConfirm(h)
	case modeOutput:
		content = m.renderOutput(h)
	case modeHelp:
		content = m.renderHelp(h)
	case modeSetup:
		content = m.renderSetup(h)
	}
	return lipgloss.Place(m.width, h, lipgloss.Center, lipgloss.Center, content)
}

// ── palette ───────────────────────────────────────────────────────────────────

func (m Model) renderPalette(h int) string {
	w, _ := m.modalSize(84)
	inner := w - 4
	items := m.pal.filtered()
	head := []string{
		styles.Header.Render(styles.Icon("search")+" ") + styles.Text.Render(m.pal.query) + styles.Primary.Render("▏") +
			"  " + styles.Muted.Render(m.pal.title),
		styles.PaneBorder.Render(strings.Repeat("─", inner)),
	}
	room := max(h-2-len(head), 1)

	// Rows with section headings (only while not filtering).
	type row struct {
		text string
		item int // -1 for a heading
	}
	var rows []row
	section := ""
	for i, it := range items {
		if m.pal.query == "" && it.section != section {
			section = it.section
			rows = append(rows, row{styles.GroupTitle.Render(section), -1})
		}
		rows = append(rows, row{m.paletteLine(it, i == m.pal.cursor, inner), i})
	}
	if len(items) == 0 {
		rows = append(rows, row{styles.Muted.Render("No matching action"), -1})
	}
	// Window around the cursor.
	sel := 0
	for i, r := range rows {
		if r.item == m.pal.cursor {
			sel = i
		}
	}
	start := 0
	if sel >= room {
		start = sel - room + 1
	}
	lines := head
	for i := start; i < len(rows) && i < start+room; i++ {
		lines = append(lines, rows[i].text)
	}
	return box(lines, w, h, false)
}

func (m Model) paletteLine(it paletteItem, sel bool, w int) string {
	a := it.a
	iconSt, labelSt := styles.Primary, styles.Text
	if a.Danger != actions.None {
		iconSt, labelSt = styles.Danger, styles.Danger
	}
	marker := "  "
	if sel {
		marker = styles.Primary.Render(styles.Icon("cursor") + " ")
		labelSt = labelSt.Bold(true)
	}
	k := ""
	if key := keyFor(a.ID); key != "" && it.t.Scope == a.Scope {
		k = styles.Key(key)
	}
	tags := ""
	switch {
	case a.Danger == actions.TypeName:
		tags = styles.Danger.Render(" " + styles.Icon("warn"))
	case a.Interactive:
		tags = styles.Muted.Render(" (terminal)")
	case a.Stream:
		tags = styles.Muted.Render(" (live)")
	}
	left := marker + iconSt.Render(styles.Icon(a.Icon)) + " " + labelSt.Render(a.Label) + tags
	room := w - lipgloss.Width(left) - lipgloss.Width(k) - 2
	help := ""
	if room > 8 && a.Help != "" && sel {
		help = "  " + styles.Muted.Render(clip(a.Help, room-2))
	}
	gap := max(w-lipgloss.Width(left+help)-lipgloss.Width(k), 1)
	return left + help + strings.Repeat(" ", gap) + k
}

// ── form ──────────────────────────────────────────────────────────────────────

func (m Model) renderForm(h int) string {
	w, _ := m.modalSize(72)
	inner := w - 4
	a := m.frm.p.action
	lines := []string{
		styles.Header.Render(styles.Icon(a.Icon)+" "+a.Label) + "  " + styles.Muted.Render(targetTitle(m.frm.p.target)),
	}
	if a.Help != "" {
		lines = append(lines, styles.Muted.Render(clip(a.Help, inner)))
	}
	lines = append(lines, "")
	labelW := 0
	for _, f := range m.frm.fields {
		labelW = max(labelW, lipgloss.Width(f.in.Label))
	}
	labelW = min(labelW+2, inner/2)
	for i, f := range m.frm.fields {
		focused := i == m.frm.focus
		label := f.in.Label
		if f.in.Required {
			label += "*"
		}
		ls := styles.Subtle
		if focused {
			ls = styles.Primary.Bold(true)
		}
		lines = append(lines, cursorMark(focused)+ls.Width(labelW).Render(clip(label, labelW))+fieldValue(f, focused, inner-labelW-2))
		if focused && f.in.Help != "" && f.in.Kind != actions.Text && f.in.Kind != actions.Path {
			lines = append(lines, strings.Repeat(" ", labelW+2)+styles.Muted.Render(clip(f.in.Help, inner-labelW-2)))
		}
	}
	if m.frm.err != "" {
		lines = append(lines, "", styles.Danger.Render(styles.Icon("error")+" "+m.frm.err))
	}
	lines = append(lines, "", styles.Muted.Render("$ homelab "+strings.Join(m.formPreview(), " ")))
	return box(lines, w, h, a.Danger != actions.None)
}

// formPreview is the command the form would run now.
func (m Model) formPreview() []string {
	in := actions.Inputs{}
	for k, v := range m.frm.p.inputs {
		in[k] = v
	}
	for _, f := range m.frm.fields {
		in[f.in.Key] = f.value()
	}
	return m.frm.p.action.Build(m.frm.p.target, in)
}

func fieldValue(f field, focused bool, w int) string {
	switch f.in.Kind {
	case actions.Bool:
		if f.on {
			return styles.Success.Render("[" + styles.Icon("ok") + "] yes")
		}
		return styles.Muted.Render("[ ] no")
	case actions.Choice:
		if f.loading {
			return styles.Muted.Render("loading…")
		}
		v := f.value()
		if v == "" {
			v = "(none)"
		}
		if len(f.choices) == 0 {
			return styles.Muted.Render("(no options)")
		}
		s := styles.Text.Render(clip(v, w-8))
		if focused {
			return styles.Primary.Render("‹ ") + s + styles.Primary.Render(" ›") +
				styles.Muted.Render(fmt.Sprintf(" %d/%d", f.ci+1, len(f.choices)))
		}
		return s
	}
	ti := f.ti
	ti.Width = max(w-1, 4)
	return ti.View()
}

// ── confirmations ─────────────────────────────────────────────────────────────

func (m Model) renderConfirm(h int) string {
	w, _ := m.modalSize(64)
	inner := w - 4
	p := m.confirm.p
	a := p.action
	lines := []string{
		styles.Danger.Bold(true).Render(styles.Icon("warn") + " " + a.Label + " · " + targetTitle(p.target)),
		"",
	}
	for _, l := range wrap(a.Help, inner) {
		lines = append(lines, styles.Text.Render(l))
	}
	lines = append(lines, "", styles.Muted.Render(clip("$ homelab "+strings.Join(a.Build(p.target, p.inputs), " "), inner)), "")
	if m.mode == modeTyped {
		lines = append(lines, "Type "+styles.Danger.Bold(true).Render(m.confirm.token)+" to confirm:")
		ti := m.confirm.ti
		ti.Width = inner - 3
		lines = append(lines, styles.Primary.Render("› ")+ti.View())
		if m.confirm.err != "" {
			lines = append(lines, styles.Danger.Render(m.confirm.err))
		}
	} else {
		lines = append(lines, styles.Key("y")+" yes, run it    "+styles.Key("n")+" cancel")
	}
	return box(lines, w, h, true)
}

// wrap breaks s into lines of at most w cells on spaces.
func wrap(s string, w int) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		if line != "" && lipgloss.Width(line+" "+word) > w {
			out = append(out, line)
			line = word
			continue
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// ── output ────────────────────────────────────────────────────────────────────

func (m Model) renderOutput(h int) string {
	w, _ := m.modalSize(100)
	res := m.out.res
	status := styles.Success.Render(styles.Icon("ok"))
	if res.err != nil {
		status = styles.Danger.Render(styles.Icon("error"))
	}
	lines := []string{
		status + " " + styles.Header.Render(res.title) + "  " +
			styles.Muted.Render(fmt.Sprintf("%3.f%%", m.out.vp.ScrollPercent()*100)),
		styles.Muted.Render("$ homelab " + strings.Join(res.argv, " ")),
	}
	lines = append(lines, strings.Split(m.out.vp.View(), "\n")...)
	return box(lines, w, h, res.err != nil)
}

// ── help ──────────────────────────────────────────────────────────────────────

// helpLines is the keymap: navigation, then each scope's shortcuts with the
// label and icon the registry gives them.
func (m Model) helpLines() []string {
	lines := []string{section("Keys")}
	for _, k := range navKeys {
		lines = append(lines, lipgloss.NewStyle().Width(12).Render(styles.Primary.Render(k[0]))+k[1])
	}
	for _, sc := range []actions.Scope{actions.Service, actions.Core, actions.CatalogEntry, actions.Layer, actions.Global} {
		lines = append(lines, "", section(scopeTitle(sc)))
		for _, s := range shortcuts[sc] {
			a, ok := actions.ByID(s.id)
			if !ok {
				continue
			}
			lines = append(lines, lipgloss.NewStyle().Width(12).Render(styles.Primary.Render(s.key))+
				styles.Icon(a.Icon)+" "+a.Label+"  "+styles.Muted.Render(a.Help))
		}
	}
	lines = append(lines, "", styles.Muted.Render(fmt.Sprintf("Every other action (%d in all) is in the palette.", len(actions.All()))))
	return lines
}

func scopeTitle(sc actions.Scope) string {
	return map[actions.Scope]string{
		actions.Service: "Service", actions.Core: "Core stack", actions.CatalogEntry: "Catalog entry",
		actions.Layer: "Network layer", actions.Global: "Anywhere (stack)",
	}[sc]
}

func (m Model) renderHelp(h int) string {
	w, _ := m.modalSize(90)
	lines := m.helpLines()
	top := min(m.helpTop, max(len(lines)-(h-2), 0))
	return box(lines[top:], w, h, false)
}

// ── setup ─────────────────────────────────────────────────────────────────────

func (m Model) renderSetup(h int) string {
	w, _ := m.modalSize(84)
	inner := w - 4
	s := m.setup
	name := s.svc
	if name == "" {
		name = "homelab"
	}
	lines := []string{styles.Header.Render(styles.Icon("gear") + " Configure " + name), ""}
	switch {
	case s.loading:
		lines = append(lines, styles.Muted.Render("Reading settings…"))
	case s.err != "":
		lines = append(lines, styles.Danger.Render(styles.Icon("error")+" "+s.err), "",
			styles.Key("ctrl+o")+" open the interactive wizard instead")
	case len(s.fields) == 0:
		lines = append(lines, styles.Muted.Render("Nothing to configure."))
	default:
		labelW := 0
		for _, f := range s.fields {
			labelW = max(labelW, lipgloss.Width(f.name)+2)
		}
		labelW = min(labelW+2, inner/2)
		var rows []string
		focusRow := 0
		for i, f := range s.fields {
			if i == s.focus {
				focusRow = len(rows)
			}
			ls := styles.Subtle
			if i == s.focus {
				ls = styles.Primary.Bold(true)
			}
			label := f.name
			if f.required {
				label += "*"
			}
			badge := ""
			if f.secret {
				badge = secretBadge(f) + " "
			}
			ti := f.ti
			ti.Width = max(inner-labelW-lipgloss.Width(badge)-3, 4)
			rows = append(rows, cursorMark(i == s.focus)+ls.Width(labelW).Render(clip(label, labelW))+badge+ti.View())
			if i == s.focus && f.desc != "" {
				rows = append(rows, strings.Repeat(" ", labelW+2)+styles.Muted.Render(clip(f.desc, inner-labelW-2)))
			}
		}
		room := max(h-2-len(lines)-2, 1)
		start := max(focusRow-room+2, 0)
		for i := start; i < len(rows) && i < start+room; i++ {
			lines = append(lines, rows[i])
		}
	}
	lines = append(lines, "", styles.Muted.Render("secrets go to the keyring over stdin · empty keeps the stored value"))
	return box(lines, w, h, false)
}

func secretBadge(f setupField) string {
	switch {
	case f.generated:
		return styles.Blue.Render(styles.Icon("secret") + "gen")
	case f.set:
		return styles.Success.Render(styles.Icon("secret") + "set")
	case f.required:
		return styles.Danger.Render(styles.Icon("secret") + "unset")
	}
	return styles.Muted.Render(styles.Icon("secret") + "unset")
}
