package dashboard

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/logs"
)

// ── fixtures ──────────────────────────────────────────────────────────────────

// fakeLayer implements just what the dashboard calls; the embedded nil
// interface panics on anything else, which is what a test wants.
type fakeLayer struct {
	network.NetworkLayer
	name, ctr string
}

func (f fakeLayer) Name() string          { return f.name }
func (f fakeLayer) ContainerName() string { return f.ctr }
func (f fakeLayer) Label() string         { return f.name + " layer" }
func (f fakeLayer) Flag() string {
	if f.name == "ts" {
		return ""
	}
	return f.name
}
func (f fakeLayer) ServiceAddresses(svc string, _ map[string]string) []network.ServiceAddress {
	return []network.ServiceAddress{{URL: "https://" + svc + "." + f.name}}
}

func fakeLayers() []network.NetworkLayer {
	return []network.NetworkLayer{
		fakeLayer{name: "ts", ctr: "tailscale"},
		fakeLayer{name: "cf", ctr: "cloudflared"},
		fakeLayer{name: "tor", ctr: "tor"},
		fakeLayer{name: "i2p", ctr: "i2pd"},
		fakeLayer{name: "ygg", ctr: "yggdrasil"},
	}
}

func stubServices() []service.Service {
	return []service.Service{
		{Name: "caddy", Installed: true, Running: 1, Total: 1, Layers: []string{"ts"}},
		{Name: "immich", Installed: true, Running: 3, Total: 3, Layers: []string{"ts", "cf", "tor", "i2p", "ygg"}},
		{Name: "jellyfin", Installed: true, Running: 1, Total: 2},
		{Name: "sonarr", Installed: true, Running: 0, Total: 1},
		{Name: "paperless", Installed: false}, // catalog-only
		{Name: "vaultwarden", Installed: false},
	}
}

func newTestModel(svcs []service.Service) Model {
	m := New(Options{
		Root:     "/nonexistent/homelab-test",
		Services: svcs,
		Catalog:  []string{"paperless", "vaultwarden"},
		Layers:   fakeLayers(),
		BuildEnv: func(string) map[string]string { return map[string]string{"DOMAIN": "example.com"} },
		CLI:      []string{"homelab-test-binary"},
	})
	m.width, m.height = 120, 40
	m.enabled = map[string]bool{"cf": true, "tor": true}
	m.core = map[string]string{"caddy": "running", "tailscale": "running", "tor": "running"}
	return m
}

// selectNamed puts the cursor on the named row of the current view.
func selectNamed(m Model, name string) Model {
	var list []service.Service
	switch m.view {
	case viewServices:
		list = m.visibleServices()
	case viewCatalog:
		list = m.visibleCatalog()
	}
	for i, s := range list {
		if s.Name == name && !isCore(&s) {
			m.cursor[m.view] = i
		}
	}
	return m
}

var specialKeys = map[string]tea.KeyType{
	"esc": tea.KeyEsc, "enter": tea.KeyEnter, "tab": tea.KeyTab, "shift+tab": tea.KeyShiftTab,
	"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"ctrl+p": tea.KeyCtrlP, "ctrl+c": tea.KeyCtrlC, "ctrl+o": tea.KeyCtrlO, "ctrl+u": tea.KeyCtrlU,
	"ctrl+d": tea.KeyCtrlD, "backspace": tea.KeyBackspace, "space": tea.KeySpace,
}

func keyMsg(k string) tea.KeyMsg {
	if t, ok := specialKeys[k]; ok {
		if t == tea.KeySpace {
			return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		}
		return tea.KeyMsg{Type: t}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func pressCmd(m Model, k string) (Model, tea.Cmd) {
	out, cmd := m.Update(keyMsg(k))
	return out.(Model), cmd
}

func press(m Model, keys ...string) Model {
	for _, k := range keys {
		m, _ = pressCmd(m, k)
	}
	return m
}

func typeText(m Model, s string) Model {
	for _, r := range s {
		m = press(m, string(r))
	}
	return m
}

func paletteIDs(m Model) []string {
	var ids []string
	for _, it := range m.pal.filtered() {
		ids = append(ids, it.a.ID)
	}
	return ids
}

// isExec reports whether cmd suspends the program for a process.
func isExec(cmd tea.Cmd) bool {
	return cmd != nil && fmt.Sprintf("%T", cmd()) == "tea.execMsg"
}

// ── basics ────────────────────────────────────────────────────────────────────

func Test_Init_ReturnsCommands(t *testing.T) {
	require.NotNil(t, newTestModel(stubServices()).Init())
}

func Test_WindowSizeMsg_SetsDimensions(t *testing.T) {
	m := newTestModel(stubServices())
	out, _ := m.Update(tea.WindowSizeMsg{Width: 99, Height: 33})
	assert.Equal(t, 99, out.(Model).width)
	assert.Equal(t, 33, out.(Model).height)
}

func Test_CtrlC_Quits(t *testing.T) {
	_, cmd := pressCmd(newTestModel(stubServices()), "ctrl+c")
	require.NotNil(t, cmd)
	assert.IsType(t, tea.QuitMsg{}, cmd())
}

func Test_View_EmptyWhenNoWidth(t *testing.T) {
	m := newTestModel(stubServices())
	m.width = 0
	assert.Equal(t, "", m.View())
}

// ── views ─────────────────────────────────────────────────────────────────────

func Test_ViewSwitching(t *testing.T) {
	m := newTestModel(stubServices())
	assert.Equal(t, viewCatalog, press(m, "tab").view)
	assert.Equal(t, viewHealth, press(m, "shift+tab").view)
	for i, v := range []view{viewServices, viewCatalog, viewNetwork, viewBackups, viewHealth} {
		assert.Equal(t, v, press(m, fmt.Sprint(i+1)).view)
	}
	assert.Equal(t, viewServices, press(m, "tab", "tab", "tab", "tab", "tab").view)
}

func Test_Services_And_Catalog_Split(t *testing.T) {
	m := newTestModel(stubServices())
	assert.True(t, isCore(&m.visibleServices()[0]), "core is pinned first")
	assert.Len(t, m.visibleServices(), 5)
	assert.Len(t, m.visibleCatalog(), 2)
}

func Test_Navigation(t *testing.T) {
	m := newTestModel(stubServices())
	assert.Equal(t, 1, press(m, "j").cursor[viewServices])
	assert.Equal(t, 0, press(m, "j", "k").cursor[viewServices])
	assert.Equal(t, 4, press(m, "G").cursor[viewServices])
	assert.Equal(t, 0, press(m, "G", "g", "g").cursor[viewServices])
	assert.Equal(t, 4, press(m, "G", "down").cursor[viewServices], "clamped at the end")
	assert.Equal(t, 0, press(m, "G", "ctrl+u").cursor[viewServices])
}

func Test_Filter(t *testing.T) {
	m := press(newTestModel(stubServices()), "/")
	require.Equal(t, modeFilter, m.mode)
	m = typeText(m, "JELL")
	require.Len(t, m.visibleServices(), 1)
	assert.Equal(t, "jellyfin", m.visibleServices()[0].Name)
	m = press(m, "backspace", "enter")
	assert.Equal(t, modeNormal, m.mode)
	assert.Equal(t, "JEL", m.filter[viewServices])
	assert.Equal(t, "", press(m, "esc").filter[viewServices])
}

func Test_Refresh_UpdatesServicesAndEnabled(t *testing.T) {
	m := newTestModel(nil)
	out, _ := m.Update(refreshedMsg{services: stubServices(), enabled: map[string]bool{"i2p": true}})
	m2 := out.(Model)
	assert.Equal(t, stubServices(), m2.services)
	assert.Equal(t, []string{"ts", "i2p"}, m2.offered())
}

func Test_ContainerDetailMsg_StaleIgnored(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
	out, _ := m.Update(containerDetailMsg{svcName: "jellyfin", details: []docker.ContainerDetail{{}}})
	assert.Nil(t, out.(Model).containerDetails)
	out, _ = m.Update(containerDetailMsg{svcName: "caddy", details: []docker.ContainerDetail{{}}})
	assert.Len(t, out.(Model).containerDetails, 1)
}

// ── palette ───────────────────────────────────────────────────────────────────

func Test_Palette_ServiceTarget(t *testing.T) {
	m := press(selectNamed(newTestModel(stubServices()), "caddy"), "enter")
	require.Equal(t, modePalette, m.mode)
	ids := paletteIDs(m)
	assert.Contains(t, ids, "service.up")
	assert.Contains(t, ids, "service.enable.cf")
	assert.NotContains(t, ids, "service.enable", "already on the tailnet")
	assert.Contains(t, ids, "global.status", "stack-wide actions follow")
	assert.NotContains(t, ids, "core.up")
	assert.Less(t, slices.Index(ids, "service.delete"), slices.Index(ids, "global.status"),
		"the selection's actions come before the stack's")

	m = typeText(m, "rest")
	it, ok := m.pal.selected()
	require.True(t, ok)
	assert.Equal(t, "service.restart", it.a.ID, "best fuzzy match first")
	m = typeText(m, "zzzz")
	assert.Empty(t, m.pal.filtered())
	assert.Equal(t, modeNormal, press(m, "esc").mode)
}

func Test_Palette_CoreTarget(t *testing.T) {
	m := newTestModel(stubServices())
	m.cursor[viewServices] = 0
	ids := paletteIDs(press(m, ":"))
	assert.Contains(t, ids, "core.up")
	assert.Contains(t, ids, "core.validate")
	assert.NotContains(t, ids, "service.up")
}

func Test_Palette_CatalogTarget(t *testing.T) {
	m := press(newTestModel(stubServices()), "2", "ctrl+p")
	ids := paletteIDs(m)
	assert.Equal(t, "catalog.add", ids[0])
	assert.NotContains(t, ids, "service.up")
}

func Test_Palette_LayerTarget(t *testing.T) {
	m := press(newTestModel(stubServices()), "3", "j", "enter") // cf: enabled, not running
	require.Equal(t, "cf", m.selectedLayer().Name())
	ids := paletteIDs(m)
	assert.Contains(t, ids, "layer.disable")
	assert.Contains(t, ids, "layer.start")
	assert.NotContains(t, ids, "layer.enable")
	assert.NotContains(t, ids, "layer.list", "cf has no list command")
}

func Test_Palette_MultiTarget(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
	m = press(m, "space", "space") // caddy and immich
	assert.Equal(t, []string{"caddy", "immich"}, m.markedNames())
	m = press(m, "enter")
	for _, it := range m.pal.items {
		if it.t.Scope == actions.Service {
			assert.True(t, it.a.Multi, "%s offered for a multi-selection", it.a.ID)
		}
	}
	assert.Contains(t, paletteIDs(m), "service.up")
	assert.NotContains(t, paletteIDs(m), "service.logs")
	assert.Empty(t, press(m, "esc", "esc").markedNames(), "esc clears marks")
}

func Test_ExposureMenu(t *testing.T) {
	m := press(selectNamed(newTestModel(stubServices()), "caddy"), "e")
	require.Equal(t, modePalette, m.mode)
	for _, it := range m.pal.items {
		assert.Equal(t, actions.GroupExposure, it.a.Group)
	}
}

func Test_FuzzyScore(t *testing.T) {
	_, ok := fuzzyScore("xyz", "Restart")
	assert.False(t, ok)
	a, _ := fuzzyScore("up", "Up")
	b, _ := fuzzyScore("up", "Pull images")
	assert.Greater(t, a, b)
}

// ── forms ─────────────────────────────────────────────────────────────────────

func Test_Form_Defaults(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
	a, _ := actions.ByID("service.logs")
	tg, _ := m.target()
	m, _ = m.start(a, tg, nil, true)
	require.Equal(t, modeForm, m.mode)
	in, err := m.frm.values()
	require.NoError(t, err)
	assert.Equal(t, "true", in["follow"])
	assert.Equal(t, "200", in["tail"])
	assert.Equal(t, []string{"logs", "--follow", "--tail", "200", "caddy"}, m.formPreview())

	m = press(m, "space") // toggle follow off
	in, _ = m.frm.values()
	assert.Equal(t, "", in["follow"])
}

func Test_Form_RequiredInput(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
	a, _ := actions.ByID("service.port")
	tg, _ := m.target()
	m, _ = m.start(a, tg, nil, false) // shortcut path still asks: required input
	require.Equal(t, modeForm, m.mode)
	m = press(m, "enter")
	assert.Equal(t, modeForm, m.mode)
	assert.Contains(t, m.frm.err, "required")
	m = press(typeText(m, "80"), "enter")
	assert.Equal(t, modeNormal, m.mode)
	assert.Equal(t, "homelab port caddy 80", m.busyMsg)
}

func Test_Form_DynamicChoicesAndBackupPrefill(t *testing.T) {
	m := newTestModel(stubServices())
	m.backups = []backup.Listing{{Dir: "/b/2026-10-01", Created: time.Unix(0, 0), Services: []string{"caddy"}}}
	m = press(m, "4")
	m, cmd := pressCmd(m, "r") // global.restore, prefilled with the selected backup
	require.Equal(t, modeForm, m.mode, "restore asks: it has a required choice")
	in, _ := m.frm.values()
	assert.Equal(t, "/b/2026-10-01", in["backup"])
	require.NotNil(t, cmd, "dynamic choices resolve in a command")

	out, _ := m.Update(choicesMsg{key: "backup", choices: []string{"/b/new", "/b/2026-10-01"}})
	m = out.(Model)
	in, _ = m.frm.values()
	assert.Equal(t, "/b/2026-10-01", in["backup"], "the prefilled value stays selected")
	m = press(m, "right")
	in, _ = m.frm.values()
	assert.NotEqual(t, "/b/2026-10-01", in["backup"])
}

// ── danger ────────────────────────────────────────────────────────────────────

func Test_Danger_Confirm(t *testing.T) {
	m := newTestModel(stubServices())
	m.cursor[viewServices] = 0 // core
	m = press(m, "x")          // core.down: Confirm
	require.Equal(t, modeConfirm, m.mode)
	assert.Equal(t, modeNormal, press(m, "n").mode)
	assert.Equal(t, 0, press(m, "n").running)
	m = press(m, "y")
	assert.Equal(t, modeNormal, m.mode)
	assert.Equal(t, 1, m.running)
	assert.Equal(t, "homelab down", m.busyMsg)
}

func Test_Danger_TypeName(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "sonarr")
	a, _ := actions.ByID("service.delete")
	tg, _ := m.target()
	m, _ = m.start(a, tg, nil, false)
	require.Equal(t, modeTyped, m.mode)
	assert.Equal(t, "sonarr", m.confirm.token)

	m = press(typeText(m, "sonar"), "enter")
	assert.Equal(t, modeTyped, m.mode, "a wrong token does not run")
	assert.Contains(t, m.confirm.err, "sonarr")
	assert.Equal(t, 0, m.running)

	m = press(typeText(m, "r"), "enter")
	assert.Equal(t, modeNormal, m.mode)
	assert.Equal(t, "homelab delete sonarr --yes", m.busyMsg)
}

func Test_Danger_TypeName_MultiTokenIsAll(t *testing.T) {
	m := press(selectNamed(newTestModel(stubServices()), "caddy"), "space", "space", "enter")
	m = typeText(m, "prune")
	m = press(m, "enter") // form: keep-volumes, keep-images
	m = press(m, "enter") // submit the form
	require.Equal(t, modeTyped, m.mode)
	assert.Equal(t, "all", m.confirm.token)
}

// ── running ───────────────────────────────────────────────────────────────────

func Test_Shortcuts_RunCLIVerbs(t *testing.T) {
	for k, want := range map[string]string{
		"u": "homelab up caddy", "s": "homelab stop caddy", "r": "homelab restart caddy",
		"x": "homelab down caddy", "U": "homelab update caddy", "b": "homelab backup caddy",
	} {
		m := press(selectNamed(newTestModel(stubServices()), "caddy"), k)
		assert.Equal(t, want, m.busyMsg, "key %s", k)
	}
	m := newTestModel(stubServices())
	m.cursor[viewServices] = 0
	assert.Equal(t, "homelab restart", press(m, "r").busyMsg, "core runs the no-service form")
}

func Test_Shortcut_Unavailable(t *testing.T) {
	m := press(selectNamed(newTestModel(stubServices()), "sonarr"), "s") // nothing running
	assert.True(t, m.toastErr)
	assert.Equal(t, 0, m.running)
}

func Test_Catalog_EnterInstalls(t *testing.T) {
	m := press(newTestModel(stubServices()), "2", "enter")
	assert.Equal(t, "homelab add paperless", m.busyMsg)
}

func Test_Interactive_ExecProcess(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
	_, cmd := pressCmd(m, "t") // shell
	assert.True(t, isExec(cmd), "shell suspends the TUI")

	_, cmd = pressCmd(m, "n") // global.new wizard
	assert.True(t, isExec(cmd))
}

func Test_ActionDone_ShowsReportOrToast(t *testing.T) {
	m := newTestModel(stubServices())
	status, _ := actions.ByID("service.status")
	up, _ := actions.ByID("service.up")

	out, _ := m.Update(actionDoneMsg{p: pending{action: status}, res: result{title: "Status", out: "all good"}})
	assert.Equal(t, modeOutput, out.(Model).mode)

	out, _ = m.Update(actionDoneMsg{p: pending{action: up}, res: result{title: "Up", out: "done"}})
	m2 := out.(Model)
	assert.Equal(t, modeNormal, m2.mode)
	assert.False(t, m2.toastErr)
	assert.Equal(t, modeOutput, press(m2, "o").mode, "output is kept for o")

	out, _ = m.Update(actionDoneMsg{p: pending{action: up}, res: result{title: "Up", out: "x\nerror: boom", err: errors.New("exit 1")}})
	assert.Equal(t, modeOutput, out.(Model).mode, "failures show their output")
	assert.Contains(t, out.(Model).toast, "error: boom")
}

func Test_Health_RunsIntoPane(t *testing.T) {
	m := press(newTestModel(stubServices()), "5")
	rows := m.healthRows()
	require.NotEmpty(t, rows)
	for _, r := range rows {
		assert.Equal(t, actions.GroupInspect, r.a.Group)
	}
	m, cmd := pressCmd(m, "enter")
	require.NotNil(t, cmd)
	out, _ := m.Update(actionDoneMsg{p: pending{action: rows[0].a, origin: viewHealth}, res: result{out: "report"}})
	m = out.(Model)
	assert.Equal(t, modeNormal, m.mode, "health output goes to its pane, not a modal")
	assert.Contains(t, m.View(), "report")
}

func Test_RunCmd_CapturesOutput(t *testing.T) {
	m := newTestModel(stubServices())
	m.opt.CLI = []string{"sh", "-c", "echo progress; echo 'error: boom' >&2; exit 1", "sh"}
	a, _ := actions.ByID("service.up")
	msg := m.runCmd(pending{action: a}, []string{"up", "x"})().(actionDoneMsg)
	require.Error(t, msg.res.err)
	assert.Equal(t, "error: boom", lastLine(msg.res.out))
}

func Test_Stream_EmbeddedLogViewer(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
	m.opt.CLI = []string{"true"}
	m, cmd := pressCmd(m, "l")
	require.Equal(t, modeLogs, m.mode)
	require.NotNil(t, cmd)
	assert.Contains(t, m.View(), "Logs")
	assert.Equal(t, 40, strings.Count(m.View(), "\n")+1)

	m, cmd = pressCmd(m, "esc")
	require.NotNil(t, cmd)
	require.IsType(t, logs.ClosedMsg{}, cmd())
	out, _ := m.Update(cmd())
	assert.Equal(t, modeNormal, out.(Model).mode)
}

// ── setup form ────────────────────────────────────────────────────────────────

func testSetupInfo() setupInfo {
	var info setupInfo
	info.Vars = append(info.Vars, struct {
		Name        string `json:"name"`
		Value       string `json:"value"`
		Required    bool   `json:"required"`
		Description string `json:"description"`
	}{Name: "PORT", Value: "8080", Required: true})
	info.Secrets = append(info.Secrets, struct {
		Name        string `json:"name"`
		Required    bool   `json:"required"`
		Description string `json:"description"`
		Set         bool   `json:"set"`
		Generated   bool   `json:"generated"`
	}{Name: "API_KEY", Set: true}, struct {
		Name        string `json:"name"`
		Required    bool   `json:"required"`
		Description string `json:"description"`
		Set         bool   `json:"set"`
		Generated   bool   `json:"generated"`
	}{Name: "DB_PASS", Generated: true, Set: true})
	return info
}

func Test_Setup_FormSavesWithoutSecretsOnArgv(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
	m, cmd := pressCmd(m, "c")
	require.Equal(t, modeSetup, m.mode)
	require.True(t, m.setup.loading)
	require.NotNil(t, cmd, "settings load in a command")

	out, _ := m.Update(setupLoadedMsg{svc: "caddy", info: testSetupInfo()})
	m = out.(Model)
	require.Len(t, m.setup.fields, 3)
	assert.Contains(t, m.View(), "set")
	assert.Contains(t, m.View(), "gen")

	m = press(m, "backspace", "backspace")
	m = typeText(m, "90")
	m = press(m, "tab")
	m = typeText(m, "s3cr3t")
	assert.NotContains(t, m.View(), "s3cr3t", "secrets are masked")

	sets, secrets := m.setup.changes()
	assert.Equal(t, []string{"PORT=8090"}, sets)
	assert.Equal(t, map[string]string{"API_KEY": "s3cr3t"}, secrets)
	args := setupSaveArgs("caddy", sets, secrets)
	assert.Equal(t, []string{"setup", "caddy", "--set", "PORT=8090", "--secrets-stdin"}, args)
	assert.NotContains(t, strings.Join(args, " "), "s3cr3t")

	m, cmd = pressCmd(m, "enter")
	assert.Equal(t, modeNormal, m.mode)
	assert.Equal(t, 1, m.running)
	require.NotNil(t, cmd)
}

func Test_Setup_WizardStillAvailable(t *testing.T) {
	m := selectNamed(newTestModel(stubServices()), "caddy")
	m = press(m, "c")
	_, cmd := pressCmd(m, "ctrl+o")
	assert.True(t, isExec(cmd), "ctrl+o hands the terminal to `homelab setup caddy`")
}

func Test_Setup_SaveCmdPipesSecrets(t *testing.T) {
	m := newTestModel(stubServices())
	m.opt.CLI = []string{"sh", "-c", `cat; echo " argv:$*"`, "sh"}
	a, _ := actions.ByID("service.setup")
	msg := m.setupSaveCmd(pending{action: a}, "caddy", nil, map[string]string{"K": "v"})().(actionDoneMsg)
	require.NoError(t, msg.res.err)
	assert.Contains(t, msg.res.out, `{"K":"v"}`)
	assert.NotContains(t, msg.res.out[strings.Index(msg.res.out, "argv:"):], `"v"`)
}

// ── rendering ─────────────────────────────────────────────────────────────────

// assertFits checks a frame is exactly h lines of at most w cells.
func assertFits(t *testing.T, v string, w, h int, msg string) {
	t.Helper()
	lines := strings.Split(v, "\n")
	assert.Equal(t, h, len(lines), "%s: line count", msg)
	for i, l := range lines {
		assert.LessOrEqual(t, lipgloss.Width(l), w, "%s: line %d too wide: %q", msg, i, l)
	}
}

// frames renders every view and every modal over a busy model.
func frames(t *testing.T, w, h int) map[string]string {
	m := newTestModel(stubServices())
	m.width, m.height = w, h
	m.backups = []backup.Listing{{Dir: "/b/1", Created: time.Unix(1, 0), Services: []string{"caddy", "immich"}, Live: true}}
	m.logLines, m.logSvcName = slices.Repeat([]string{strings.Repeat("log line ", 30)}, 30), "caddy"
	m = selectNamed(m, "caddy")
	out := map[string]string{}
	for v := view(0); v < numViews; v++ {
		mv := press(m, fmt.Sprint(v+1))
		out[viewNames[v]] = mv.View()
	}
	out["palette"] = press(m, "enter").View()
	out["help"] = press(m, "?").View()
	out["form"] = press(m, "enter", "l", "o", "g", "s", "enter").View()
	mm, _ := m.Update(actionDoneMsg{p: pending{action: actions.All()[0]}, res: result{title: "t", out: strings.Repeat("output line\n", 100)}})
	out["output"] = press(mm.(Model), "o").View()
	out["typed"] = press(selectNamed(m, "sonarr"), "enter", "d", "e", "l", "e", "t", "e", "enter", "enter").View()
	sm := press(m, "c")
	ms, _ := sm.Update(setupLoadedMsg{svc: "caddy", info: testSetupInfo()})
	out["setup"] = ms.(Model).View()
	return out
}

func Test_View_FitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}, {100, 30}, {50, 12}} {
		for name, v := range frames(t, size[0], size[1]) {
			assertFits(t, v, size[0], size[1], fmt.Sprintf("%s at %dx%d", name, size[0], size[1]))
		}
	}
}

func Test_View_80x24_Content(t *testing.T) {
	f := frames(t, 80, 24)
	assert.Contains(t, f["Services"], "homelab")
	assert.Contains(t, f["Services"], "immich")
	assert.Contains(t, f["Services"], "https://caddy.ts")
	assert.Contains(t, f["Catalog"], "paperless")
	assert.Contains(t, f["Network"], "core stack")
	assert.Contains(t, f["Network"], "tor")
	assert.Contains(t, f["Backups"], "caddy")
	assert.Contains(t, f["Health"], "Status overview")
	assert.Contains(t, f["palette"], "Restart")
	assert.Contains(t, f["help"], "Keys")
	assert.Contains(t, f["typed"], "Type")
	assert.Contains(t, f["form"], "Lines from the end")
}

func Test_Help_GeneratedFromRegistry(t *testing.T) {
	text := strings.Join(newTestModel(nil).helpLines(), "\n")
	for _, list := range shortcuts {
		for _, s := range list {
			a, ok := actions.ByID(s.id)
			require.True(t, ok, "shortcut %s → unknown action %s", s.key, s.id)
			assert.Contains(t, text, a.Label)
		}
	}
}

// ── parity ────────────────────────────────────────────────────────────────────

// Test_Parity_EveryActionReachable walks every row of every view (and a
// multi-selection) in two stack configurations — extensions all on, all off —
// and checks that (1) the palette for each selection offers exactly what the
// registry says applies, plus the stack-wide actions, and (2) together they
// reach every action in actions.All().
func Test_Parity_EveryActionReachable(t *testing.T) {
	reached := map[string]bool{}
	for _, enabled := range []map[string]bool{
		{"cf": true, "tor": true, "i2p": true, "ygg": true},
		{},
	} {
		base := newTestModel(stubServices())
		base.enabled = enabled
		base.core = map[string]string{"caddy": "running", "tailscale": "running", "tor": "running"}
		for v := view(0); v < numViews; v++ {
			m := press(base, fmt.Sprint(v+1))
			for i := range m.rowCount(v) {
				m.cursor[v] = i
				collect(t, m, reached)
			}
		}
		multi := press(selectNamed(base, "caddy"), "space", "space", "space")
		collect(t, multi, reached)
	}
	for _, a := range actions.All() {
		assert.True(t, reached[a.ID], "action %s is not reachable from any TUI selection", a.ID)
	}
}

func collect(t *testing.T, m Model, reached map[string]bool) {
	t.Helper()
	tg, ok := m.target()
	if !ok {
		return
	}
	p := press(m, ":")
	require.Equal(t, modePalette, p.mode)
	got := map[string]bool{}
	for _, it := range p.pal.items {
		got[it.a.ID] = true
		reached[it.a.ID] = true
	}
	for _, a := range append(actions.For(tg), actions.For(m.globalTarget())...) {
		assert.True(t, got[a.ID], "palette for %s lacks %s", targetTitle(tg), a.ID)
	}
	// Every palette entry is runnable on its target.
	for _, it := range p.pal.items {
		assert.True(t, it.a.Can(it.t), "%s offered but not available", it.a.ID)
	}
}

// ── clip ──────────────────────────────────────────────────────────────────────

func Test_Clip(t *testing.T) {
	assert.Equal(t, "abc", clip("abc", 10))
	assert.Equal(t, "abcd…", clip("abcdefgh", 5))
	r := clip("café-postgres-container", 5)
	assert.True(t, utf8.ValidString(r))
	assert.Equal(t, "café…", r)
	assert.Equal(t, "abcdef", clip("abcdef", 0))
}
