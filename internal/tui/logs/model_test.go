package logs

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReadyModel builds a Model with a small, initialized viewport and
// enough lines that scrolling is meaningful, without starting a real
// log-stream subprocess.
func newReadyModel(t *testing.T, lines int) Model {
	t.Helper()
	m := Model{following: true}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m = updated.(Model)
	require.True(t, m.ready)

	for range lines {
		updated, _ = m.Update(logLineMsg{line: strings.Repeat("x", 10)})
		m = updated.(Model)
	}
	return m
}

// TestAutoFollow_ScrollUpFromBottom_DisablesFollowImmediately is a
// regression test: checking AtBottom() before applying the scroll keystroke
// meant a single scroll-up press from the bottom left `following` true —
// it took a second press to catch up, since the first press's check ran
// against the pre-scroll (still "at bottom") position.
func TestAutoFollow_ScrollUpFromBottom_DisablesFollowImmediately(t *testing.T) {
	m := newReadyModel(t, 50) // far more lines than the 10-row viewport
	require.True(t, m.following, "should be following after new lines arrive")
	require.True(t, m.viewport.AtBottom())

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = updated.(Model)

	assert.False(t, m.following, "scrolling away from the bottom must disable follow on the same keypress")
}

// A scroll key that can't actually move the viewport (already at the only
// possible position, e.g. too few lines to scroll) must not spuriously
// disable follow.
func TestAutoFollow_ScrollKeyThatCannotMove_LeavesFollowEnabled(t *testing.T) {
	m := newReadyModel(t, 2) // fits entirely within the 10-row viewport
	require.True(t, m.following)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = updated.(Model)

	assert.True(t, m.following, "AtBottom() is still true when there's nothing to scroll")
}

func TestStop_SafeWhenStopFnNil(t *testing.T) {
	m := Model{}
	assert.NotPanics(t, func() { m.Stop() })
}
