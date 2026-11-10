package gui

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/clistate"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
)

// The GUI's refresh reads everything through the CLI source — `status
// --json` and `backup --list --json`, forwarded to the leader — never the
// local engine or config dir.
func TestLoad_ThroughCLISource(t *testing.T) {
	st := clistate.Status{
		Edge:       clistate.Edge{Reachable: true},
		Extensions: []clistate.Extension{{Name: "cf", Container: "homelab_cloudflared", Enabled: true, State: "running"}},
		Services: []clistate.Service{{Name: "web", Layers: []string{"ts", "cf"}, Running: 1, Total: 2,
			Addresses: map[string][]network.ServiceAddress{"ts": {{URL: "https://web.home.example.com"}}}}},
	}
	var ran [][]string
	src := clistate.CLI{Argv: []string{"homelab"}, Run: func(_ context.Context, argv []string) ([]byte, error) {
		ran = append(ran, argv)
		if slices.Equal(argv, []string{"homelab", "backup", "--list", "--json"}) {
			return []byte(`[{"dir":"/var/lib/homelab-backups/b1","services":["web"]}]`), nil
		}
		require.Equal(t, []string{"homelab", "status", "--json"}, argv)
		return json.Marshal(st)
	}}

	l := load(Options{Source: src, Catalog: []string{"paperless"}, AllLayers: layers.Static()})
	require.Empty(t, l.discoverErr)
	require.Len(t, l.services, 2)
	assert.Equal(t, "web", l.services[0].Name)
	assert.Equal(t, 1, l.services[0].Running)
	assert.Equal(t, 2, l.services[0].Total)
	assert.False(t, l.services[1].Installed, "catalog-only paperless")
	assert.Equal(t, []string{"ts", "cf"}, l.layers)
	assert.Equal(t, ContainerState{Name: agentContainer, State: "running"}, l.core[0])
	require.Len(t, l.exts, 1)
	assert.True(t, l.exts[0].Running)
	assert.Len(t, ran, 1, "the status read does not list backups")
	bl, berr := loadBackups(Options{Source: src})
	require.Empty(t, berr)
	require.Len(t, bl, 1)
	assert.Len(t, ran, 2)

	addrs := addressesOf(&l.services[0], l.layerObjs)
	require.Len(t, addrs, 2)
	assert.Equal(t, "https://web.home.example.com", addrs[0].Addrs[0].URL)
}

// The status read never waits on the backups listing, and neither read
// starts again while one is in flight.
func TestRefresher_BackupsDoNotDelayStatusAndNoOverlap(t *testing.T) {
	var r refresher
	release := make(chan struct{})
	backupsStarted := make(chan struct{})
	var backupsRuns, statusRuns atomic.Int32
	slowBackups := func() {
		backupsRuns.Add(1)
		close(backupsStarted)
		<-release
	}
	status := func() { statusRuns.Add(1) }

	done := make(chan struct{})
	go func() { r.run(status, slowBackups); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the status refresh waited on the backups listing")
	}
	<-backupsStarted
	assert.Equal(t, int32(1), statusRuns.Load())

	// A second refresh while the listing hangs: status runs, no second
	// listing starts.
	r.run(status, slowBackups)
	assert.Equal(t, int32(2), statusRuns.Load())
	assert.Equal(t, int32(1), backupsRuns.Load(), "no overlapping backups listing")

	// A status read still in flight is not started again either.
	require.True(t, r.status.begin())
	r.run(status, func() {})
	assert.Equal(t, int32(2), statusRuns.Load(), "no overlapping status read")
	r.status.end()
	close(release)
}
