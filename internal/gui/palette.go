package gui

import (
	"sort"
	"strings"
	"unicode"

	"github.com/groot/homelab/internal/actions"
)

// paletteItem is one command palette row: an action on a target.
type paletteItem struct {
	Action actions.Action
	Target actions.Target
	// Context names the target ("jellyfin", "stack", "core", "tor").
	Context string
}

func (p paletteItem) text() string {
	return p.Action.Label + " " + p.Context + " " + p.Action.Group
}

// paletteItems are the actions offered for the current selection, followed by
// the stack-wide ones (deduplicated by ID+context).
func paletteItems(current actions.Target, currentName string, global actions.Target) []paletteItem {
	var out []paletteItem
	seen := map[string]bool{}
	add := func(t actions.Target, ctx string) {
		for _, a := range actions.For(t) {
			key := a.ID + "|" + ctx
			if !seen[key] {
				seen[key] = true
				out = append(out, paletteItem{Action: a, Target: t, Context: ctx})
			}
		}
	}
	if current.Scope != actions.Global {
		add(current, currentName)
	}
	add(global, "stack")
	return out
}

// fuzzyScore matches query as a case-insensitive subsequence of text. ok is
// false when it does not match; a higher score is a better match: consecutive
// runs and matches at word starts score more, and so do earlier matches.
func fuzzyScore(query, text string) (score int, ok bool) {
	q := []rune(strings.ToLower(strings.TrimSpace(query)))
	if len(q) == 0 {
		return 0, true
	}
	t := []rune(strings.ToLower(text))
	qi, run := 0, 0
	for ti := 0; ti < len(t) && qi < len(q); ti++ {
		if q[qi] == ' ' { // a space in the query matches any word break
			qi++
			run = 0
			if qi == len(q) {
				break
			}
		}
		if t[ti] != q[qi] {
			run = 0
			continue
		}
		s := 1
		if ti == 0 || !unicode.IsLetter(t[ti-1]) && !unicode.IsDigit(t[ti-1]) {
			s += 8 // word start
		}
		run++
		s += 4 * (run - 1) // consecutive
		if ti < 16 {
			s += 1
		}
		score += s
		qi++
	}
	if qi < len(q) {
		return 0, false
	}
	return score, true
}

// fuzzyFilter returns the items matching query, best first; ties keep their
// original order. An empty query returns all items unchanged.
func fuzzyFilter(items []paletteItem, query string) []paletteItem {
	if strings.TrimSpace(query) == "" {
		return items
	}
	type scored struct {
		item  paletteItem
		score int
	}
	var hits []scored
	for _, it := range items {
		// The label alone scores double, so "up" prefers "Up" over "Pull images".
		ls, lok := fuzzyScore(query, it.Action.Label)
		ts, tok := fuzzyScore(query, it.text())
		if !lok && !tok {
			continue
		}
		hits = append(hits, scored{it, max(ls*2, ts)})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]paletteItem, len(hits))
	for i, h := range hits {
		out[i] = h.item
	}
	return out
}
