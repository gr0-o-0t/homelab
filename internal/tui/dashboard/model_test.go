package dashboard

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
)

// stubEnvBuilder returns a minimal env map for tests.
func stubEnvBuilder(svcName string) map[string]string {
	return map[string]string{"DOMAIN": "home.example.com", "HOME_SUBDOMAIN": "home"}
}

func stubServices() []service.Service {
	return []service.Service{
		{Name: "caddy", Installed: true, Running: 1, Total: 1, HasCaddyConf: true, Enabled: true},
		{Name: "immich", Installed: true, Running: 3, Total: 3, HasCaddyConf: true, Enabled: true},
		{Name: "jellyfin", Installed: true, Running: 1, Total: 2, HasCaddyConf: true, Enabled: false},
		{Name: "sonarr", Installed: true, Running: 0, Total: 1, HasCaddyConf: false},
		{Name: "paperless", Installed: false}, // catalog-only
		{Name: "vaultwarden", Installed: false},
	}
}

func newTestModel(svcs []service.Service) Model {
	return New(
		"/test/repo",
		nil, // no docker client
		svcs,
		[]string{"paperless", "vaultwarden"},
		[]network.NetworkLayer{},
		stubEnvBuilder,
	)
}

// ── Init ──────────────────────────────────────────────────────────────────────

func Test_Init_ReturnsCommands(t *testing.T) {
	m := newTestModel(stubServices())
	cmd := m.Init()
	require.NotNil(t, cmd, "Init should return a tea.Cmd")
}

// ── Window resize ─────────────────────────────────────────────────────────────

func Test_WindowSizeMsg_SetsDimensions(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 0, 0

	resModel, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	require.NotNil(t, resModel)

	updated, ok := resModel.(Model)
	require.True(t, ok)
	assert.Equal(t, 120, updated.width)
	assert.Equal(t, 40, updated.height)
	assert.Nil(t, cmd, "WindowSizeMsg should not trigger a command")
}

// ── Keyboard navigation ───────────────────────────────────────────────────────

func Test_NavigateUpDown_CursorMoves(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 2

	// Move up
	up, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.NotNil(t, up)
	m2 := up.(Model)
	assert.Equal(t, 1, m2.cursor, "cursor should move up")

	// Move down
	down, _ := m2.Update(tea.KeyMsg{Type: tea.KeyDown})
	m3 := down.(Model)
	assert.Equal(t, 2, m3.cursor, "cursor should move down")

	// j = down
	jModel, _ := m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m4 := jModel.(Model)
	assert.Equal(t, 3, m4.cursor)

	// k = up
	kModel, _ := m4.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m5 := kModel.(Model)
	assert.Equal(t, 2, m5.cursor)
}

func Test_CursorBounds_Clamped(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 5 // last visible (paperless is at index 5, 0-based: 5 items)

	// Try moving past end
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 5, m2.(Model).cursor, "cursor should not go past last item")
}

// ── Filter ────────────────────────────────────────────────────────────────────

func Test_Filter_ToggleAndInput(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal

	// Press / to enter filter
	filtered, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m2 := filtered.(Model)
	assert.Equal(t, stateFilterInput, m2.state)
	assert.Equal(t, "", m2.filter)

	// Type "son"
	typed, _ := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m3 := typed.(Model)
	typed2, _ := m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	m4 := typed2.(Model)
	typed3, _ := m4.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m5 := typed3.(Model)
	assert.Equal(t, "son", m5.filter)

	// Backspace
	back, _ := m5.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m6 := back.(Model)
	assert.Equal(t, "so", m6.filter)

	// Enter to exit filter
	done, _ := m6.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m7 := done.(Model)
	assert.Equal(t, stateNormal, m7.state)
}

func Test_Filter_VisibleServices(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateFilterInput
	m.filter = "son"

	visible := m.visibleServices()
	require.Len(t, visible, 1)
	assert.Equal(t, "sonarr", visible[0].Name)
}

func Test_Filter_CaseInsensitive(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateFilterInput
	m.filter = "JELLY"

	visible := m.visibleServices()
	require.Len(t, visible, 1)
	assert.Equal(t, "jellyfin", visible[0].Name)
}

func Test_Filter_EmptyShowsAll(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.filter = ""
	visible := m.visibleServices()
	assert.Equal(t, len(m.services), len(visible))
}

// ── State transitions ─────────────────────────────────────────────────────────

func Test_Refresh_SetsServices(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40

	refreshed, cmd := m.Update(refreshedMsg{services: stubServices()})
	m2 := refreshed.(Model)
	assert.Equal(t, stubServices(), m2.services)
	assert.NotNil(t, cmd, "refresh should trigger log fetch")
}

func Test_BusyOp_SetsStateBusy(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.busyOp("Working…")

	assert.Equal(t, stateBusy, m.state)
	assert.Equal(t, "Working…", m.busyMsg)
	assert.Equal(t, "", m.lastMsg)
	assert.Equal(t, "", m.lastErr)
}

func Test_OpDoneMsg_ClearsBusy(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateBusy
	m.busyMsg = "Working…"

	done, _ := m.Update(opDoneMsg{msg: "done"})
	m2 := done.(Model)
	assert.Equal(t, "done", m2.lastMsg)
	assert.Equal(t, "", m2.lastErr)
}

func Test_OpErrMsg_SetsError(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateBusy
	m.busyMsg = "Working…"

	errModel, _ := m.Update(opErrMsg{err: assert.AnError})
	m2 := errModel.(Model)
	assert.Equal(t, stateNormal, m2.state)
	assert.Contains(t, m2.lastErr, assert.AnError.Error())
}

// ── Prompt states ─────────────────────────────────────────────────────────────

func Test_EnablePrompt_KeyE(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 0 // caddy — installed & enabled

	prompted, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m2 := prompted.(Model)
	assert.Equal(t, stateEnablePrompt, m2.state)
}

func Test_EnablePrompt_CatalogServiceIgnored(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 4 // paperless — catalog only, not installed

	// Pressing 'e' on a non-installed service should be a no-op
	prompted, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m2 := prompted.(Model)
	assert.Equal(t, stateNormal, m2.state, "catalog-only service should not enter enable prompt")
}

// ── Quit ──────────────────────────────────────────────────────────────────────

func Test_CtrlC_Quits(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40

	quitModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	require.NotNil(t, cmd)

	_, ok := quitModel.(Model)
	assert.True(t, ok)
}

// ── View ──────────────────────────────────────────────────────────────────────

func Test_View_EmptyWhenNoWidth(t *testing.T) {
	m := newTestModel(stubServices())
	m.width = 0
	assert.Equal(t, "", m.View())
}

func Test_View_RendersWithServices(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	v := m.View()
	assert.NotEmpty(t, v)
	assert.Contains(t, v, "homelab")
	assert.Contains(t, v, "caddy")
	assert.Contains(t, v, "immich")
}

// ── Vim bindings ──────────────────────────────────────────────────────────────

func Test_GG_JumpsToTop(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 4

	// First g sets up sequence tracking
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	assert.Equal(t, 4, m2.(Model).cursor, "single g should not move cursor")

	// Second g triggers gg → jump to top
	m3, _ := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	assert.Equal(t, 0, m3.(Model).cursor, "gg should jump to top")
}

func Test_GG_AtTopStaysAtTop(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 0

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	m3, _ := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	assert.Equal(t, 0, m3.(Model).cursor, "gg at top should stay at top")
}

func Test_G_JumpsToBottom(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 0

	// G jumps to last item
	shiftG := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}
	m2, _ := m.Update(shiftG)
	m3 := m2.(Model)
	visible := m3.visibleServices()
	assert.Equal(t, len(visible)-1, m3.cursor, "G should jump to bottom")
}

func Test_CtrlU_HalfPageUp(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 10

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m3 := m2.(Model)
	// 120 width × 40 height → body height ≈ 38, half ≈ 19
	// cursor 10 - 19 → clamped to 0
	assert.Equal(t, 0, m3.cursor, "ctrl+u should scroll half page up")
}

func Test_CtrlD_HalfPageDown(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 0

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m3 := m2.(Model)
	visible := m3.visibleServices()
	// 120×40 → body height ≈ 38, half ≈ 19
	// cursor 0 + 19 → clamped to last visible if past end
	if len(visible) > 19 {
		assert.Equal(t, 19, m3.cursor, "ctrl+d should scroll half page down")
	} else {
		assert.Equal(t, len(visible)-1, m3.cursor, "ctrl+d should clamp to last item")
	}
}

func Test_CtrlD_ClampsAtBottom(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 5

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m3 := m2.(Model)
	// already at/near bottom, ctrl+d should clamp to last
	visible := m3.visibleServices()
	assert.Equal(t, len(visible)-1, m3.cursor)
}

func Test_VimBindings_OnlyInListPane(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m.cursor = 2
	m.focused = paneDetail // not list

	// G should not move cursor when detail pane is focused
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	assert.Equal(t, 2, m2.(Model).cursor, "vim bindings should only work in list pane")
}
