package styles

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/actions"
)

// Every action icon has a glyph in both sets, and every glyph is one cell
// wide, so columns line up whatever icon a row shows.
func Test_Icons_CoverActionsAndAreOneCell(t *testing.T) {
	for set, glyphs := range IconSets() {
		for _, name := range actions.Icons {
			g, ok := glyphs[name]
			if assert.True(t, ok, "%s set has no glyph for %q", set, name) {
				assert.Equal(t, 1, lipgloss.Width(g), "%s glyph for %q (%q)", set, name, g)
			}
		}
		for name, g := range glyphs {
			assert.Equal(t, 1, lipgloss.Width(g), "%s glyph for %q (%q)", set, name, g)
		}
	}
}

func Test_Icon_NerdFontEnv(t *testing.T) {
	t.Setenv("HOMELAB_NERD_FONT", "")
	assert.Equal(t, "▶", Icon("play"))
	t.Setenv("HOMELAB_NERD_FONT", "1")
	assert.Equal(t, "", Icon("play"))
	assert.Equal(t, "•", Icon("no-such-icon"))
}
