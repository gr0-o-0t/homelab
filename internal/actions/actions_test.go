package actions

import (
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/service"
)

// TestMain keeps these tests off the real Docker daemon; see cmd/main_test.go.
func TestMain(m *testing.M) {
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-test-docker.sock")
	os.Exit(m.Run())
}

func TestRegistry_WellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range All() {
		assert.False(t, seen[a.ID], "duplicate ID %s", a.ID)
		seen[a.ID] = true
		assert.NotEmpty(t, a.Label, a.ID)
		assert.Contains(t, Groups, a.Group, a.ID)
		assert.Contains(t, Icons, a.Icon, a.ID)
		assert.NotNil(t, a.Args, a.ID)
		assert.NotEmpty(t, a.Commands, a.ID)
		if a.Danger != None {
			assert.NotEmpty(t, a.Help, "%s: a confirmation dialog needs Help text", a.ID)
		}
		keys := map[string]bool{}
		for _, in := range a.Inputs {
			assert.False(t, keys[in.Key], "%s: duplicate input %s", a.ID, in.Key)
			keys[in.Key] = true
			if in.Kind == Choice {
				assert.True(t, in.Source != "" || len(in.Choices) > 0, "%s: choice %s has no options", a.ID, in.Key)
			}
		}
	}
}

func ids(as []Action) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.ID
	}
	return out
}

func TestFor_ServiceStateGatesActions(t *testing.T) {
	layers := []string{"ts", "cf"}
	stopped := ServiceTarget(service.Service{Name: "web", Installed: true, Total: 1}, layers)
	got := ids(For(stopped))
	assert.Contains(t, got, "service.start")
	assert.NotContains(t, got, "service.stop")
	assert.NotContains(t, got, "service.shell", "no shell into a stopped container")
	assert.Contains(t, got, "service.enable")
	assert.Contains(t, got, "service.enable.cf")
	assert.NotContains(t, got, "service.enable.tor", "tor is not enabled in config.yaml")
	assert.NotContains(t, got, "service.disable")

	running := ServiceTarget(service.Service{Name: "web", Installed: true, Total: 1, Running: 1, Layers: []string{"ts", "cf"}}, layers)
	got = ids(For(running))
	assert.Contains(t, got, "service.stop")
	assert.Contains(t, got, "service.shell")
	assert.NotContains(t, got, "service.enable")
	assert.Contains(t, got, "service.disable")
	assert.Contains(t, got, "service.disable.cf")
	assert.NotContains(t, got, "service.enable.all", "already exposed on every offered layer")

	catalog := ServiceTarget(service.Service{Name: "web"}, layers)
	assert.Equal(t, []string{"catalog.add"}, ids(For(catalog)))
}

func TestFor_OrderedByGroup(t *testing.T) {
	as := For(ServiceTarget(service.Service{Name: "web", Installed: true, Total: 1, Running: 1}, []string{"ts"}))
	require.NotEmpty(t, as)
	last := -1
	for _, a := range as {
		r := slices.Index(Groups, a.Group)
		assert.GreaterOrEqual(t, r, last, "%s out of group order", a.ID)
		last = r
	}
}

func TestBuild_FillsDefaultsAndAppendsYes(t *testing.T) {
	svc := Target{Scope: Service, Name: "web"}

	logs, _ := ByID("service.logs")
	assert.Equal(t, []string{"logs", "--follow", "--tail", "200", "web"}, logs.Build(svc, nil))

	prune, _ := ByID("service.prune")
	assert.Equal(t, TypeName, prune.Danger)
	assert.Equal(t, "web", prune.Token(svc))
	assert.Equal(t, []string{"prune", "web", "--keep-volumes", "--yes"}, prune.Build(svc, Inputs{"keep-volumes": "true"}))

	up, _ := ByID("global.up")
	assert.Equal(t, []string{"up", "--all"}, up.Build(GlobalTarget(nil), nil))
	assert.Equal(t, []string{"up", "--group", "media", "--build"}, up.Build(GlobalTarget(nil), Inputs{"group": "media", "build": "true"}))
	gp, _ := ByID("global.prune")
	assert.Equal(t, "all", gp.Token(GlobalTarget(nil)))

	coreUp, _ := ByID("core.up")
	assert.Equal(t, []string{"up"}, coreUp.Build(CoreTarget(nil), nil))

	exec, _ := ByID("service.exec")
	assert.Equal(t, []string{"exec", "--no-tty", "--user", "root", "web", "ls", "-la"},
		exec.Build(svc, Inputs{"command": "ls -la", "user": "root"}))
}

func TestLayerTarget(t *testing.T) {
	off := ids(For(LayerTarget("tor", false, false)))
	assert.Contains(t, off, "layer.enable")
	assert.NotContains(t, off, "layer.disable")
	on := ids(For(LayerTarget("cf", true, true)))
	assert.Contains(t, on, "layer.disable")
	assert.Contains(t, on, "layer.stop")
	assert.NotContains(t, on, "layer.list", "cf has no list command")
}

func TestChoices_Groups(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(root+"/config.yaml", []byte("groups:\n  media: [a]\n  infra: [b]\n"), 0o600))
	got, err := Choices(root, Input{Kind: Choice, Source: SourceGroups})
	require.NoError(t, err)
	assert.Equal(t, []string{"infra", "media"}, got)

	got, err = Choices(root, Input{Kind: Choice, Source: SourceBackups})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestBuild_MultiSelection(t *testing.T) {
	sel := Target{Scope: Service, Names: []string{"web", "db"}}
	prune, _ := ByID("service.prune")
	assert.True(t, prune.Multi)
	assert.Equal(t, []string{"prune", "web", "db", "--yes"}, prune.Build(sel, nil))
	assert.Equal(t, "all", prune.Token(sel), "the CLI asks for \"all\" when several are pruned")
	backup, _ := ByID("service.backup")
	assert.Equal(t, []string{"backup", "web", "db"}, backup.Build(sel, nil))
	up, _ := ByID("service.up")
	assert.Equal(t, []string{"up", "web", "db"}, up.Build(sel, nil))

	logs, _ := ByID("service.logs")
	assert.False(t, logs.Can(sel), "single-service actions are not offered for a multi-selection")
	assert.Equal(t, []string{"logs", "--follow", "--tail", "200", "web"},
		logs.Build(Target{Scope: Service, Names: []string{"web"}}, nil), "a one-service selection is that service")
}
