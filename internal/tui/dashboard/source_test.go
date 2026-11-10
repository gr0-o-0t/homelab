package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/clistate"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/network"
)

// fakeCLI answers the CLI invocations the dashboard makes with canned JSON,
// recording each argv: what selfCLI would get back from the leader.
func fakeCLI(t *testing.T, st clistate.Status, ran *[][]string) clistate.CLI {
	t.Helper()
	return clistate.CLI{Argv: []string{"homelab", "--no-color"}, Run: func(_ context.Context, argv []string) ([]byte, error) {
		*ran = append(*ran, argv)
		args := argv[2:]
		switch {
		case slices.Equal(args, []string{"status", "--json"}):
			return json.Marshal(st)
		case len(args) == 3 && args[0] == "status" && args[2] == "--json":
			for _, s := range st.Services {
				if s.Name == args[1] {
					return json.Marshal(s)
				}
			}
		case slices.Equal(args, []string{"backup-list", "--json"}):
			return []byte("[]"), nil
		}
		t.Fatalf("unexpected CLI call %v", argv)
		return nil, nil
	}}
}

// The dashboard shows what `homelab status --json` reports — services,
// counts, enabled extensions, the edge — and reads a service's tasks with
// `status <svc> --json`; nothing local.
func TestDashboard_FromCLIStatus(t *testing.T) {
	st := clistate.Status{
		Edge: clistate.Edge{Reachable: true},
		Extensions: []clistate.Extension{
			{Name: "cf", Container: "cloudflared", Enabled: true, State: "running"},
		},
		Services: []clistate.Service{
			{Name: "web", Layers: []string{"ts"}, Running: 2, Total: 3,
				Addresses: map[string][]network.ServiceAddress{"ts": {{URL: "https://web.home.example.com"}}},
				Containers: []docker.ContainerDetail{{ContainerSummary: docker.ContainerSummary{
					Name: "web", State: "running", Node: "laptop"}}}},
		},
	}
	var ran [][]string
	m := New(Options{Source: fakeCLI(t, st, &ran), Catalog: []string{"paperless", "web"},
		Layers: fakeLayers(), CLI: []string{"homelab"}})
	m.width, m.height = 120, 40

	out, _ := m.Update(m.refreshCmd()())
	m = out.(Model)
	require.Len(t, m.services, 2, "installed web, then catalog-only paperless")
	assert.Equal(t, "web", m.services[0].Name)
	assert.True(t, m.services[0].Installed)
	assert.Equal(t, 2, m.services[0].Running)
	assert.Equal(t, 3, m.services[0].Total)
	assert.False(t, m.services[1].Installed)
	assert.True(t, m.enabled["cf"])
	assert.Equal(t, "running", m.core[agentContainer])
	assert.Equal(t, "running", m.core["cloudflared"])

	m = selectNamed(m, "web")
	out, _ = m.Update(m.fetchInspectCmd()())
	m = out.(Model)
	require.Len(t, m.containerDetails, 1)
	assert.Equal(t, "laptop", m.containerDetails[0].Node)

	view := m.View()
	assert.Contains(t, view, "2/3 running")
	assert.Contains(t, view, "https://web.home.example.com")
	assert.Contains(t, ran, []string{"homelab", "--no-color", "status", "--json"})
	assert.Contains(t, ran, []string{"homelab", "--no-color", "status", "web", "--json"})
}

// The Backups view and a backup choice list backups with `backup-list
// --json`, the lock-free command, never `backup --list` (which waits behind
// any running up or backup).
func TestDashboard_ListsBackupsWithBackupList(t *testing.T) {
	var ran [][]string
	m := New(Options{Source: fakeCLI(t, clistate.Status{}, &ran), Layers: fakeLayers(), CLI: []string{"homelab"}})
	m.backupsCmd()()
	m.choicesCmd(actions.Input{Key: "backup", Kind: actions.Choice, Source: actions.SourceBackups})()
	want := []string{"homelab", "--no-color", "backup-list", "--json"}
	assert.Equal(t, [][]string{want, want}, ran)
}

// A refresh still in flight is not started again: every tick while the
// leader hangs adds nothing.
func TestDashboard_NoOverlappingRefresh(t *testing.T) {
	m := New(Options{Source: clistate.CLI{Argv: []string{"homelab"}}, Layers: fakeLayers(), CLI: []string{"homelab"}})
	require.True(t, m.refreshing, "Init's read is in flight")
	out, cmd := m.Update(coreTickMsg{})
	m = out.(Model)
	assert.True(t, m.refreshing)
	// Only the next tick is scheduled: no status read.
	assert.NotNil(t, cmd)
	m2, c := m.startRefresh()
	assert.Nil(t, c, "no second read while one is in flight")
	m, _ = m2, c

	out, _ = m.Update(refreshErrMsg{err: errors.New("no answer from the leader within 15s")})
	m = out.(Model)
	assert.False(t, m.refreshing)
	_, c = m.startRefresh()
	assert.NotNil(t, c, "the next tick reads again")
}

// After a failed refresh the last data stays on screen, marked stale.
func TestDashboard_FailedRefreshIsStale(t *testing.T) {
	m := newTestModel(stubServices())
	out, _ := m.Update(refreshedMsg{services: stubServices()})
	m = out.(Model)
	require.False(t, m.lastRefresh.IsZero())
	out, _ = m.Update(refreshErrMsg{err: errors.New("no answer from the leader within 15s")})
	m = out.(Model)
	view := m.View()
	assert.Contains(t, view, "immich", "the last data is still shown")
	assert.Contains(t, view, "stale — last updated "+m.lastRefresh.Format("15:04:05"))
	assert.Contains(t, view, "refresh failing: no answer from the leader")

	out, _ = m.Update(refreshedMsg{services: stubServices()})
	assert.NotContains(t, out.(Model).View(), "stale")
}
