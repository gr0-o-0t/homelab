package gui

import (
	"encoding/json"
	"slices"
	"testing"

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
	src := clistate.CLI{Argv: []string{"homelab"}, Run: func(argv []string) ([]byte, error) {
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
	require.Len(t, l.backups, 1)
	assert.Len(t, ran, 2)

	addrs := addressesOf(&l.services[0], l.layerObjs)
	require.Len(t, addrs, 2)
	assert.Equal(t, "https://web.home.example.com", addrs[0].Addrs[0].URL)
}
