package logs

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func Test_New(t *testing.T) {
	m := New("/tmp/test-homelab", "caddy", nil)
	assert.NotNil(t, m)
}

func Test_View_NotEmpty(t *testing.T) {
	m := New("/tmp/test-homelab", "caddy", nil)
	v := m.View()
	assert.NotEmpty(t, v)
}

func Test_Init(t *testing.T) {
	m := New("/tmp/test-homelab", "caddy", nil)
	cmd := m.Init()
	assert.NotNil(t, cmd)
}

func Test_Quit(t *testing.T) {
	m := New("/tmp/test-homelab", "caddy", nil)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	_, ok := updated.(Model)
	assert.True(t, ok)
	assert.NotNil(t, cmd)
}
