package list

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/service"
)

func stubServices() []service.Service {
	return []service.Service{
		{Name: "caddy", Installed: true, Running: 1, Total: 1, Enabled: true},
		{Name: "immich", Installed: true, Running: 3, Total: 3, Enabled: true},
		{Name: "jellyfin", Installed: true, Running: 1, Total: 2, Enabled: false},
		{Name: "sonarr", Installed: true, Running: 0, Total: 1, Enabled: false},
	}
}

func newTestModel(svcs []service.Service) Model {
	return New("/test/repo", nil, svcs, 80, 24, nil)
}

// ── Init ──────────────────────────────────────────────────────────────────────

func Test_List_Init(t *testing.T) {
	m := newTestModel(stubServices())
	cmd := m.Init()
	assert.Nil(t, cmd)
}

// ── Window resize ─────────────────────────────────────────────────────────────

func Test_List_WindowSize(t *testing.T) {
	m := newTestModel(stubServices())
	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m2 := updated.(Model)
	assert.Equal(t, 100, m2.width)
	assert.Equal(t, 30, m2.height)
	assert.Nil(t, cmd)
}

// ── Quit ──────────────────────────────────────────────────────────────────────

func Test_List_Quit_Q(t *testing.T) {
	m := newTestModel(stubServices())
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	_, ok := updated.(Model)
	assert.True(t, ok)
	assert.NotNil(t, cmd, "q should produce a tea.Quit command")
}

func Test_List_Quit_CtrlC(t *testing.T) {
	m := newTestModel(stubServices())
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	_, ok := updated.(Model)
	assert.True(t, ok)
	assert.NotNil(t, cmd, "ctrl+c should produce a tea.Quit command")
}

// ── Selection for logs ────────────────────────────────────────────────────────

func Test_List_LogSelection(t *testing.T) {
	m := newTestModel(stubServices())
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m2 := updated.(Model)
	assert.Equal(t, "caddy", m2.SelectedForLogs, "should select first service for logs")
	assert.NotNil(t, cmd, "l should produce a tea.Quit command")
}

// ── New service ───────────────────────────────────────────────────────────────

func Test_List_NewService(t *testing.T) {
	m := newTestModel(stubServices())
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m2 := updated.(Model)
	assert.True(t, m2.SelectedForNew)
	assert.NotNil(t, cmd, "n should produce a tea.Quit command")
}

// ── Refresh ───────────────────────────────────────────────────────────────────

func Test_List_Refresh_SetsBusy(t *testing.T) {
	m := newTestModel(stubServices())
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	m2 := updated.(Model)
	assert.Equal(t, stateBusy, m2.uiState)
	assert.Contains(t, m2.busyMsg, "Refreshing")
	assert.NotNil(t, cmd)
}

// ── Busy ignores keys ─────────────────────────────────────────────────────────

func Test_List_BusyState_IgnoresKeys(t *testing.T) {
	m := newTestModel(stubServices())
	m.uiState = stateBusy
	m.busyMsg = "Working…"

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := updated.(Model)
	assert.Equal(t, stateBusy, m2.uiState, "busy state should ignore quit")
	assert.Nil(t, cmd)
}

// ── Async messages ────────────────────────────────────────────────────────────

func Test_List_RefreshedMsg(t *testing.T) {
	m := newTestModel(stubServices())
	m.uiState = stateBusy
	m.busyMsg = "Refreshing…"

	updated, _ := m.Update(refreshedMsg{services: stubServices()})
	m2 := updated.(Model)
	assert.Equal(t, stateIdle, m2.uiState)
	assert.Equal(t, stubServices(), m2.services)
}

func Test_List_OpDoneMsg(t *testing.T) {
	m := newTestModel(stubServices())
	m.uiState = stateBusy
	m.busyMsg = "Working…"

	updated, cmd := m.Update(opDoneMsg{msg: "done"})
	m2 := updated.(Model)
	assert.Equal(t, "done", m2.lastMsg)
	assert.NotNil(t, cmd, "opDone should trigger refresh")
}

func Test_List_OpErrMsg(t *testing.T) {
	m := newTestModel(stubServices())
	m.uiState = stateBusy
	m.busyMsg = "Working…"

	updated, _ := m.Update(opErrMsg{err: assert.AnError})
	m2 := updated.(Model)
	assert.Equal(t, stateIdle, m2.uiState)
	assert.Contains(t, m2.lastErr, assert.AnError.Error())
}

// ── View ──────────────────────────────────────────────────────────────────────

func Test_List_View_EmptyWhenNoWidth(t *testing.T) {
	m := newTestModel(stubServices())
	m.width = 0
	assert.Equal(t, "", m.View())
}

func Test_List_View_Renders(t *testing.T) {
	m := newTestModel(stubServices())
	m.width = 80
	v := m.View()
	assert.NotEmpty(t, v)
	assert.Contains(t, v, "Homelab")
	assert.Contains(t, v, "caddy")
	assert.Contains(t, v, "immich")
}
