package dashboard

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/docker"
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
		nil,
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
	last := len(m.visibleServices()) - 1
	m.cursor = last

	// Try moving past end
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, last, m2.(Model).cursor, "cursor should not go past last item")
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
	assert.Equal(t, len(m.services)+1, len(visible), "every service plus the pinned core row")
	assert.True(t, isCore(&visible[0]))
}

// ── State transitions ─────────────────────────────────────────────────────────

func Test_Refresh_SetsServices(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
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

func Test_EnablePrompt_CatalogServiceIgnored(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.state = stateNormal
	m = selectNamed(m, "paperless") // catalog only, not installed

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

func Test_ContainerDetailMsg_UpdatesDetailPane(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m = selectNamed(m, "caddy")

	upd, _ := m.Update(containerDetailMsg{
		svcName: "caddy",
		details: []docker.ContainerDetail{
			{ContainerSummary: docker.ContainerSummary{Name: "caddy", State: "running", Image: "caddy:latest"}},
		},
	})
	m2 := upd.(Model)
	assert.NotNil(t, m2.containerDetails)
	assert.Len(t, m2.containerDetails, 1)
}

func Test_ContainerDetailMsg_StaleIgnored(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.cursor = 0

	upd, _ := m.Update(containerDetailMsg{
		svcName: "jellyfin",
		details: []docker.ContainerDetail{
			{ContainerSummary: docker.ContainerSummary{Name: "jf", State: "running"}},
		},
	})
	m2 := upd.(Model)
	assert.Nil(t, m2.containerDetails, "detail for non-selected service should be ignored")
}

func Test_CoreStatusMsg_TorI2pYggPreserved(t *testing.T) {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40

	upd, _ := m.Update(coreStatusMsg{
		"tailscale": "running", "caddy": "running",
		"tor": "running", "i2p": "exited",
		"yggdrasil": "running",
	})
	m2 := upd.(Model)
	assert.Equal(t, "running", m2.core["tor"])
	assert.Equal(t, "exited", m2.core["i2p"])
	assert.Equal(t, "running", m2.core["yggdrasil"])
}

// ── clip ──────────────────────────────────────────────────────────────────────

func Test_Clip_ShorterThanLimit_Unchanged(t *testing.T) {
	assert.Equal(t, "abc", clip("abc", 10))
}

func Test_Clip_ASCIITruncation(t *testing.T) {
	assert.Equal(t, "abcd…", clip("abcdefgh", 5))
}

// Regression: clip() used to slice by byte index (s[:n-1]), which can cut a
// multi-byte UTF-8 rune in half. Container names, log lines, and error text
// can all contain non-ASCII characters.
func Test_Clip_MultiByteRunes_NotCorrupted(t *testing.T) {
	s := "café-postgres-container" // "é" is 2 bytes in UTF-8
	result := clip(s, 5)
	assert.True(t, utf8.ValidString(result), "clip must not produce invalid UTF-8")
	assert.Equal(t, "café…", result)
}

func Test_Clip_EmojiNotCorrupted(t *testing.T) {
	s := "🎉🎉🎉🎉🎉🎉🎉🎉" // each emoji is a multi-byte rune
	result := clip(s, 4)
	assert.True(t, utf8.ValidString(result))
	assert.Equal(t, "🎉🎉🎉…", result)
}

func Test_Clip_ZeroOrNegativeLimit_ReturnsUnchanged(t *testing.T) {
	assert.Equal(t, "abcdef", clip("abcdef", 0))
	assert.Equal(t, "abcdef", clip("abcdef", -1))
}

// ── Layers and CLI actions ────────────────────────────────────────────────────

// fakeLayer implements just what the dashboard calls; the embedded nil
// interface panics on anything else, which is what a test wants.
type fakeLayer struct {
	network.NetworkLayer
	name, ctr string
}

func (f fakeLayer) Name() string          { return f.name }
func (f fakeLayer) ContainerName() string { return f.ctr }
func (f fakeLayer) ServiceAddresses(svc string, _ map[string]string) []network.ServiceAddress {
	return []network.ServiceAddress{{URL: "https://" + svc + "." + f.name}}
}

func layeredModel() Model {
	m := newTestModel(stubServices())
	m.width, m.height = 120, 40
	m.layers = []network.NetworkLayer{
		fakeLayer{name: "ts", ctr: "tailscale"},
		fakeLayer{name: "cf", ctr: "cloudflared"},
		fakeLayer{name: "tor", ctr: "tor"},
	}
	return selectNamed(m, "caddy")
}

// selectNamed puts the cursor on the named row.
func selectNamed(m Model, name string) Model {
	for i, s := range m.visibleServices() {
		if s.Name == name && !isCore(&s) {
			m.cursor = i
		}
	}
	return m
}

func press(m Model, k string) Model {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	if k == "esc" {
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	}
	out, _ := m.Update(msg)
	return out.(Model)
}

func choiceKeys(cs []layerChoice) (keys []string) {
	for _, c := range cs {
		keys = append(keys, c.key)
	}
	return keys
}

func Test_EnablePrompt_OffersConfiguredLayers(t *testing.T) {
	m := press(layeredModel(), "e") // caddy: ts enabled
	require.Equal(t, stateEnablePrompt, m.state)
	assert.Equal(t, []string{"p", "c", "t"}, choiceKeys(m.promptChoices(m.selectedService())))
}

func Test_DisablePrompt_OffersOnlyActiveLayers(t *testing.T) {
	m := press(layeredModel(), "d")
	require.Equal(t, stateDisablePrompt, m.state)
	assert.Equal(t, []string{"p"}, choiceKeys(m.promptChoices(m.selectedService())))
}

func Test_DisablePrompt_NothingActive(t *testing.T) {
	m := layeredModel()
	m = selectNamed(m, "jellyfin") // no layers enabled
	m = press(m, "d")
	assert.Equal(t, stateNormal, m.state)
	assert.Contains(t, m.lastErr, "no network layers")
}

func Test_Prompt_UnofferedKeyKeepsPrompt(t *testing.T) {
	m := press(press(layeredModel(), "d"), "c") // cf is not active on caddy
	assert.Equal(t, stateDisablePrompt, m.state)
}

func Test_Prompt_RunsMatchingCLICommand(t *testing.T) {
	m := press(press(layeredModel(), "e"), "c")
	assert.Equal(t, stateBusy, m.state)
	assert.Equal(t, "homelab enable caddy --cf…", m.busyMsg)

	m = press(press(layeredModel(), "e"), "a")
	assert.Equal(t, "homelab enable caddy --cf --tor…", m.busyMsg)
}

func Test_LifecycleKeys_RunCLIVerbs(t *testing.T) {
	for k, verb := range map[string]string{"u": "up", "s": "stop", "r": "restart", "x": "down"} {
		m := press(layeredModel(), k)
		assert.Equal(t, "homelab "+verb+" caddy…", m.busyMsg, "key %s", k)
	}
}

func Test_Filter_EscClears(t *testing.T) {
	m := press(press(press(layeredModel(), "/"), "i"), "esc")
	assert.Equal(t, stateNormal, m.state)
	assert.Equal(t, "", m.filter)
}

func Test_Header_UsesShortLayerNames(t *testing.T) {
	v := layeredModel().View()
	assert.Contains(t, v, "ts")
	assert.Contains(t, v, "https://caddy.ts")
	assert.NotContains(t, v, "tunnel")
}

func Test_CLICmd_ReportsLastOutputLine(t *testing.T) {
	m := newTestModel(stubServices())
	m.cli = []string{"sh", "-c", "echo progress; echo 'error: boom' >&2; exit 1", "sh"}
	msg := m.cliCmd("ok", "up", "x")()
	require.IsType(t, opErrMsg{}, msg)
	assert.Equal(t, "error: boom", msg.(opErrMsg).output)

	m.cli = []string{"true"}
	assert.Equal(t, opDoneMsg{msg: "ok"}, m.cliCmd("ok", "up", "x")())
}

func Test_View_FitsTerminalHeight(t *testing.T) {
	for _, help := range []bool{false, true} {
		m := layeredModel()
		m.width, m.height, m.help = 80, 12, help
		m.logLines, m.logSvcName = make([]string, 30), "caddy"
		for i := range m.logLines {
			m.logLines[i] = "log line"
		}
		assert.Equal(t, 12, strings.Count(m.View(), "\n")+1, "help=%v", help)
	}
}

func Test_CoreRow_RunsNoServiceCommands(t *testing.T) {
	m := layeredModel()
	m.cursor = 0
	require.True(t, isCore(m.selectedService()), "core is pinned first")

	assert.Equal(t, "homelab restart…", press(m, "r").busyMsg)
	assert.Equal(t, "homelab update…", press(m, "U").busyMsg)

	stopped := press(m, "x")
	assert.Equal(t, stateNormal, stopped.state, "down on the core is shell-only")
	assert.Contains(t, stopped.lastErr, "homelab down")

	assert.Equal(t, stateNormal, press(m, "e").state, "core has no layers to enable")
	assert.True(t, press(m, "l").SelectedCoreLogs)
}

func Test_UpdateKey_RunsUpdate(t *testing.T) {
	assert.Equal(t, "homelab update caddy…", press(layeredModel(), "U").busyMsg)
}
