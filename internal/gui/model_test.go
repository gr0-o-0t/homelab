package gui

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// ── icons ─────────────────────────────────────────────────────────────────────

func TestGlyph_EveryActionIconHasOne(t *testing.T) {
	for _, name := range actions.Icons {
		_, ok := actionGlyphs[name]
		assert.True(t, ok, "actions.Icons %q has no glyph", name)
	}
	for _, a := range actions.All() {
		assert.NotEqual(t, glyphGeneric, Glyph(a.Icon), "%s uses icon %q without a glyph", a.ID, a.Icon)
	}
	assert.Equal(t, glyphGeneric, Glyph("no-such-icon"))
}

func TestGlyph_LayersHaveDistinctGlyphs(t *testing.T) {
	seen := map[string]string{}
	for _, l := range layers.Static() {
		g := layerGlyph(l.Name())
		assert.NotContains(t, seen, g, "%s and %s share a glyph", l.Name(), seen[g])
		seen[g] = l.Name()
	}
}

// ── colours ───────────────────────────────────────────────────────────────────

func TestColors_MatchTerminalPalette(t *testing.T) {
	for gui, tui := range map[string]string{
		hexPrimary: string(styles.ColPrimary), hexSuccess: string(styles.ColSuccess),
		hexWarning: string(styles.ColWarning), hexError: string(styles.ColError),
		hexMuted: string(styles.ColMuted), hexText: string(styles.ColText),
		hexBorder: string(styles.ColBorder), hexAccent: string(styles.ColAccent),
	} {
		assert.True(t, strings.EqualFold(gui, tui), "gui %s != tui %s", gui, tui)
	}
	assert.Equal(t, rgba{1, 1, 1, 1}, hexColor("#FFFFFF"))
	assert.Equal(t, rgba{1, 0, 1, 1}, hexColor("nope"))
}

// ── surfaces, grouping, filtering ─────────────────────────────────────────────

var allLayers = []string{"ts", "cf", "tor", "i2p", "ygg"}

func fixtureServices() []service.Service {
	return []service.Service{
		{Name: "web", Installed: true, Running: 1, Total: 1, Layers: []service.LayerName{"ts"},
			Containers: []docker.ContainerDetail{{}}},
		{Name: "db", Installed: true, Running: 1, Total: 2, Layers: []service.LayerName{"ts", "tor"}},
		{Name: "idle", Installed: true},
		{Name: "gone", Installed: true, Total: 1},
		{Name: "everywhere", Installed: true, Running: 1, Total: 1,
			Layers: []service.LayerName{"ts", "cf", "tor", "i2p", "ygg"}},
		{Name: "jellyfin"}, // catalog entry
	}
}

func fixtureExts() []extState {
	return []extState{
		{Name: "cf", Enabled: true, Running: true}, {Name: "tor", Enabled: true},
		{Name: "i2p"}, {Name: "ygg", Enabled: true, Running: true},
	}
}

func ids(list []actions.Action) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.ID
	}
	return out
}

func TestSplit_PartitionsForExactly(t *testing.T) {
	for _, tgt := range screenTargets(fixtureServices(), allLayers, fixtureExts()) {
		s := split(tgt)
		var union []string
		union = append(append(append(union, ids(s.Toolbar)...), ids(s.Network)...), ids(s.Rest)...)
		assert.ElementsMatch(t, ids(actions.For(tgt)), union, "scope %s %q", tgt.Scope, tgt.Name)
		for _, a := range s.Toolbar {
			assert.Equal(t, actions.None, a.Danger, "danger action %s on the toolbar", a.ID)
		}
	}
}

func TestSplit_ServiceToolbar(t *testing.T) {
	tgt := actions.ServiceTarget(fixtureServices()[0], allLayers)
	s := split(tgt)
	tb := ids(s.Toolbar)
	for _, want := range []string{"service.up", "service.stop", "service.restart", "service.down", "service.update", "service.logs"} {
		assert.Contains(t, tb, want)
	}
	assert.NotContains(t, tb, "service.delete")
	assert.Contains(t, ids(s.Network), "service.disable")
	for _, a := range s.Network {
		assert.Equal(t, actions.GroupExposure, a.Group)
	}
	// Core: caddy/ts logs are not the toolbar's logs button.
	core := ids(split(actions.CoreTarget(allLayers)).Toolbar)
	assert.Contains(t, core, "core.logs")
	assert.NotContains(t, core, "core.caddy.logs")
	assert.NotContains(t, core, "core.down", "core.down asks for confirmation; it is not a quick button")
}

func TestGrouped_FollowsGroupOrder(t *testing.T) {
	g := grouped(actions.For(actions.ServiceTarget(fixtureServices()[0], allLayers)))
	require.NotEmpty(t, g)
	last := -1
	for _, grp := range g {
		i := slices.Index(actions.Groups, grp.Name)
		assert.Greater(t, i, last, "group %s out of order", grp.Name)
		last = i
		assert.NotEmpty(t, grp.Actions)
		for _, a := range grp.Actions {
			assert.Equal(t, grp.Name, a.Group)
		}
	}
	assert.Nil(t, grouped(nil))
}

func TestTouchesCommand_PicksBackupActions(t *testing.T) {
	got := ids(filterBy(actions.For(actions.GlobalTarget(allLayers)), func(a actions.Action) bool {
		return touchesCommand(a, "backup", "restore")
	}))
	assert.ElementsMatch(t, []string{"global.backup", "global.backup.list", "global.restore"}, got)
}

func TestMultiTarget_OffersOnlyMultiActions(t *testing.T) {
	svcs := fixtureServices()
	tgt := multiTarget(svcs, []string{"web", "db"}, allLayers)
	assert.Equal(t, 2, tgt.Running)
	assert.Equal(t, 3, tgt.Total)
	assert.Equal(t, []string{"ts"}, tgt.Exposed, "only layers both are exposed on")
	acts := actions.For(tgt)
	require.NotEmpty(t, acts)
	for _, a := range acts {
		assert.True(t, a.Multi, "%s offered for a multi-selection", a.ID)
		args := a.Build(tgt, nil)
		i := slices.Index(args, "web")
		require.GreaterOrEqual(t, i, 0, a.ID)
		assert.Equal(t, "db", args[i+1], "%s passes every selected service", a.ID)
	}
}

func TestLayerToggle_FindsEnableAndDisable(t *testing.T) {
	svc := fixtureServices()[1] // db: ts + tor
	tgt := actions.ServiceTarget(svc, allLayers)
	on, act := layerToggle(tgt, "tor")
	assert.True(t, on)
	require.NotNil(t, act)
	assert.Equal(t, []string{"disable", "--tor", "db"}, act.Build(tgt, nil))

	on, act = layerToggle(tgt, "cf")
	assert.False(t, on)
	require.NotNil(t, act)
	assert.Equal(t, []string{"enable", "--cf", "db"}, act.Build(tgt, nil))

	_, act = layerToggle(actions.ServiceTarget(svc, []string{"ts"}), "cf")
	assert.Nil(t, act, "cf is not offered when the extension is off")

	for _, id := range layerToggleIDs(allLayers) {
		_, ok := actions.ByID(id)
		assert.True(t, ok, id)
	}
}

func TestNeedsForm(t *testing.T) {
	get := func(id string) actions.Action { a, ok := actions.ByID(id); require.True(t, ok, id); return a }
	assert.False(t, needsForm(get("service.up"), true), "quick Up runs with defaults")
	assert.True(t, needsForm(get("service.up"), false), "Up has a --build input")
	assert.True(t, needsForm(get("service.port"), true), "required input")
	assert.True(t, needsForm(get("core.down"), true), "confirm")
	assert.True(t, needsForm(get("service.delete"), true), "type name")
	assert.False(t, needsForm(get("service.stop"), false))

	port := get("service.port")
	assert.Equal(t, []string{"Container port"}, missingInputs(port, defaultInputs(port)))
	assert.Empty(t, missingInputs(port, actions.Inputs{"port": "80"}))
	assert.Equal(t, "200", defaultInputs(get("service.logs"))["tail"])
}

func TestServiceState(t *testing.T) {
	s := fixtureServices()
	assert.Equal(t, "running", stateLabel(s[0]))
	assert.Equal(t, "1/2 running", stateLabel(s[1]))
	assert.Equal(t, "not created", stateLabel(s[2]))
	assert.Equal(t, "stopped", stateLabel(s[3]))
	assert.Equal(t, "available", stateLabel(s[5]))
	assert.True(t, matchesFilter(s[1], "db tor"))
	assert.False(t, matchesFilter(s[1], "db cf"))
	assert.True(t, matchesFilter(s[1], ""))
}

// ── parity ────────────────────────────────────────────────────────────────────

// TestParity_EveryActionReachable: every registry action is offered on some
// GUI target, across representative states (a running, a partial, a stopped
// and a never-started service, a catalog entry, extensions on/off/running).
func TestParity_EveryActionReachable(t *testing.T) {
	reached := map[string]bool{}
	for _, tgt := range screenTargets(fixtureServices(), allLayers, fixtureExts()) {
		s := split(tgt)
		for _, part := range [][]actions.Action{s.Toolbar, s.Network, s.Rest} {
			for _, a := range part {
				reached[a.ID] = true
			}
		}
	}
	for _, a := range actions.All() {
		assert.True(t, reached[a.ID], "action %s is not reachable from any GUI target", a.ID)
	}
}

// ── palette ───────────────────────────────────────────────────────────────────

func TestFuzzy(t *testing.T) {
	_, ok := fuzzyScore("rst", "Restart")
	assert.True(t, ok)
	_, ok = fuzzyScore("xyz", "Restart")
	assert.False(t, ok)
	a, _ := fuzzyScore("up", "Up")
	b, _ := fuzzyScore("up", "Pull images updated")
	assert.Greater(t, a, b, "word start + consecutive beats a scattered match")

	tgt := actions.ServiceTarget(fixtureServices()[0], allLayers)
	items := paletteItems(tgt, "web", actions.GlobalTarget(allLayers))
	assert.Equal(t, len(items), len(fuzzyFilter(items, "  ")))
	got := fuzzyFilter(items, "restart")
	require.NotEmpty(t, got)
	assert.Equal(t, "service.restart", got[0].Action.ID, "the selection's action ranks before the stack's")
	assert.Equal(t, "web", got[0].Context)
	got = fuzzyFilter(items, "back up all")
	require.NotEmpty(t, got)
	assert.Equal(t, "global.backup", got[0].Action.ID)
	assert.Empty(t, fuzzyFilter(items, "qqqqzz"))

	// The stack alone: no duplicates.
	g := actions.GlobalTarget(allLayers)
	assert.Len(t, paletteItems(g, "stack", g), len(actions.For(g)))
}

// ── setup form ────────────────────────────────────────────────────────────────

const setupDoc = `Homelab Setup
{
  "service": "web",
  "vars": [
    {"name": "WEB_PORT", "value": "8080", "required": true, "description": "Port"},
    {"name": "WEB_TITLE", "value": "", "required": false}
  ],
  "secrets": [
    {"name": "API_KEY", "required": true, "set": false},
    {"name": "DB_PASSWORD", "required": true, "set": false, "generated": true},
    {"name": "TOKEN", "required": false, "set": true}
  ]
}`

func TestSetupForm_RoundTrip(t *testing.T) {
	f, err := parseSetup([]byte(setupDoc))
	require.NoError(t, err)
	assert.Equal(t, "web", f.Service)
	require.Len(t, f.Vars, 2)
	require.Len(t, f.Secrets, 3)
	assert.False(t, f.Dirty())
	assert.Equal(t, []string{"API_KEY"}, f.Missing())
	assert.Equal(t, "unset", f.Secrets[0].status())
	assert.Equal(t, "generated", f.Secrets[1].status())
	assert.Equal(t, "set", f.Secrets[2].status())

	f.Vars[0].Value = "9090"
	f.Vars[1].Value = "a=b, c"
	f.Secrets[0].Value = "s3cr3t'\"value"
	assert.True(t, f.Dirty())
	assert.Empty(t, f.Missing())
	assert.Equal(t, "will change", f.Secrets[0].status())

	args, stdin := f.Save()
	assert.Equal(t, []string{"setup", "web", "--set=WEB_PORT=9090", "--set=WEB_TITLE=a=b, c", "--secrets-stdin"}, args)
	for _, a := range args {
		assert.NotContains(t, a, "s3cr3t", "a secret value reached argv")
	}
	var sec map[string]string
	require.NoError(t, json.Unmarshal(stdin, &sec))
	assert.Equal(t, map[string]string{"API_KEY": "s3cr3t'\"value"}, sec)
}

func TestSetupForm_VarsOnlyAndRoot(t *testing.T) {
	f, err := parseSetup([]byte(`{"vars":[{"name":"DOMAIN","value":"a.com","required":true}],"secrets":[]}`))
	require.NoError(t, err)
	f.Vars[0].Value = "b.com"
	args, stdin := f.Save()
	assert.Equal(t, []string{"setup", "--set=DOMAIN=b.com"}, args)
	assert.Nil(t, stdin)
	assert.Equal(t, []string{"setup", "--json"}, loadSetupArgs(""))
	assert.Equal(t, []string{"setup", "web", "--json"}, loadSetupArgs("web"))

	_, err = parseSetup([]byte("not json"))
	assert.Error(t, err)
}

// ── terminal ──────────────────────────────────────────────────────────────────

func TestTerminalArgv(t *testing.T) {
	cmd := []string{"/bin/homelab", "--config-dir", "/c", "exec", "web", "sh"}
	only := func(names ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			if slices.Contains(names, n) {
				return "/usr/bin/" + n, nil
			}
			return "", errors.New("not found")
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	held := append([]string{"sh", "-c", holdScript, "homelab"}, cmd...)

	argv, ok := terminalArgv(cmd, env(nil), only("xterm", "kitty"))
	require.True(t, ok)
	assert.Equal(t, append([]string{"/usr/bin/kitty"}, held...), argv, "kitty comes before xterm")

	argv, ok = terminalArgv(cmd, env(map[string]string{"TERMINAL": "alacritty"}), only("alacritty", "kitty"))
	require.True(t, ok)
	assert.Equal(t, append([]string{"/usr/bin/alacritty", "-e"}, held...), argv, "$TERMINAL wins")

	argv, ok = terminalArgv(cmd, env(map[string]string{"TERMINAL": "missing"}), only("gnome-terminal"))
	require.True(t, ok)
	assert.Equal(t, append([]string{"/usr/bin/gnome-terminal", "--"}, held...), argv)

	argv, _ = terminalArgv(cmd, env(nil), only("wezterm"))
	assert.Equal(t, []string{"/usr/bin/wezterm", "start", "--"}, argv[:3])

	_, ok = terminalArgv(cmd, env(nil), only())
	assert.False(t, ok)

	assert.Equal(t, `homelab exec web 'echo hi' 'it'\''s' ''`, shellQuote([]string{"homelab", "exec", "web", "echo hi", "it's", ""}))
}

// ── output ────────────────────────────────────────────────────────────────────

func TestCleanLineAndCap(t *testing.T) {
	assert.Equal(t, "done", cleanLine("\x1b[32m 50%\rdone\x1b[0m\r"))
	assert.Equal(t, "a    b", cleanLine("a\tb"))
	var lines []string
	for i := 0; i < maxOutputLines+10; i++ {
		lines = appendCapped(lines, "x")
	}
	assert.Len(t, lines, maxOutputLines)
}
