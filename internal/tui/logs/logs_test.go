package logs

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func Test_Quit(t *testing.T) {
	m := New("caddy", []string{"true"})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	_, ok := updated.(Model)
	assert.True(t, ok)
	assert.NotNil(t, cmd)
}
