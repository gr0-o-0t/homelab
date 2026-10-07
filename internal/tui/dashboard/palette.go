package dashboard

import (
	"sort"
	"strings"
	"unicode"

	"github.com/groot/homelab/internal/actions"
)

// paletteItem is one runnable entry: an action on a target.
type paletteItem struct {
	a       actions.Action
	t       actions.Target
	section string // group heading
}

// palette is the command palette: every action applicable to the selection,
// then the stack-wide ones, fuzzy-filtered.
type palette struct {
	title   string
	items   []paletteItem
	query   string
	cursor  int
	prefill actions.Inputs
}

// paletteItems are the actions for t, grouped by actions.Groups order, then —
// unless t is the stack itself — every Global action under "Stack" headings.
// group, when set, keeps only that group (the exposure menu).
func (m Model) paletteItems(t actions.Target, group string) []paletteItem {
	var out []paletteItem
	targetActs := actions.For(t)
	for i := range targetActs {
		a := &targetActs[i]
		if group == "" || a.Group == group {
			out = append(out, paletteItem{a: *a, t: t, section: a.Group})
		}
	}
	if t.Scope == actions.Global || group != "" {
		return out
	}
	g := m.globalTarget()
	acts := actions.For(g)
	for i := range acts {
		a := &acts[i]
		out = append(out, paletteItem{a: *a, t: g, section: "Stack · " + a.Group})
	}
	return out
}

// openPalette opens the palette on the current selection.
func (m Model) openPalette(group string) Model {
	t, ok := m.target()
	if !ok {
		t = m.globalTarget()
	}
	title := targetTitle(t)
	if group != "" {
		title += " · " + group
	}
	m.pal = palette{title: title, items: m.paletteItems(t, group), prefill: m.prefill()}
	m.mode = modePalette
	return m
}

// filtered are the items matching the query: all of them in group order when
// the query is empty, else best match first.
func (p palette) filtered() []paletteItem {
	if p.query == "" {
		return p.items
	}
	type scored struct {
		it    paletteItem
		score int
		i     int
	}
	var hits []scored
	for i := range p.items {
		it := &p.items[i]
		text := it.a.Label + " " + it.a.Group + " " + it.a.ID
		if s, ok := fuzzyScore(p.query, text); ok {
			hits = append(hits, scored{*it, s, i})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	out := make([]paletteItem, len(hits))
	for i := range hits {
		h := &hits[i]
		out[i] = h.it
	}
	return out
}

func (p palette) selected() (paletteItem, bool) {
	f := p.filtered()
	if p.cursor < 0 || p.cursor >= len(f) {
		return paletteItem{}, false
	}
	return f[p.cursor], true
}

// fuzzyScore matches query as a case-insensitive subsequence of text. Runs of
// consecutive characters and matches at word starts score higher.
func fuzzyScore(query, text string) (int, bool) {
	q := []rune(strings.ToLower(strings.TrimSpace(query)))
	t := []rune(strings.ToLower(text))
	if len(q) == 0 {
		return 0, true
	}
	score, qi, prev := 0, 0, -2
	for i := 0; i < len(t) && qi < len(q); i++ {
		if q[qi] == ' ' {
			qi++ // spaces in the query separate words; they need no match
			if qi == len(q) {
				break
			}
		}
		if t[i] != q[qi] {
			continue
		}
		score++
		if prev == i-1 {
			score += 5
		}
		if i == 0 || !unicode.IsLetter(t[i-1]) {
			score += 8
		}
		prev = i
		qi++
	}
	if qi < len(q) {
		return 0, false
	}
	return score - len(t)/16, true // shorter texts win ties
}
