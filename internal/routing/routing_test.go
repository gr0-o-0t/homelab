package routing_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/network/tailscale"
	"github.com/groot/homelab/internal/routing"
)

func writeSvc(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "services", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	for f, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte(content), 0o600))
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test path
	require.NoError(t, err)
	return string(data)
}

// A service's one sites file holds every enabled layer's blocks, in registry
// order whatever order the state lists them in.
func TestSync_OneFileAllLayersRegistryOrder(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	require.NoError(t, exposure.Save(root, "gitea", exposure.State{Layers: []string{"cf", "ts"}}))

	require.NoError(t, routing.Sync(root, layers.New(root, nil, nil), "gitea"))
	got := read(t, configgen.SitesFile(root, "gitea"))
	ts := strings.Index(got, "gitea.{$HOME_SUBDOMAIN}.{$DOMAIN} {")
	cf := strings.Index(got, "http://gitea.{$DOMAIN} {")
	require.True(t, ts >= 0 && cf >= 0, got)
	assert.Less(t, ts, cf, "ts blocks come first")
	assert.Equal(t, 2, strings.Count(got, "reverse_proxy gitea:3000"))
}

// The stored --name and --ports are what the file is rendered with.
func TestSync_UsesStoredNameAndPorts(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n  - admin:9000\n"})
	require.NoError(t, exposure.Save(root, "gitea",
		exposure.State{Layers: []string{"ts"}, Name: "git", Ports: []string{"default"}}))

	require.NoError(t, routing.Sync(root, layers.New(root, nil, nil), "gitea"))
	got := read(t, configgen.SitesFile(root, "gitea"))
	assert.Contains(t, got, "git.{$HOME_SUBDOMAIN}.{$DOMAIN} {")
	assert.NotContains(t, got, "admin.", "admin was not selected")
}

// On no layer, there is no file.
func TestSync_NoLayersRemovesFile(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	require.NoError(t, exposure.Save(root, "gitea", exposure.State{Layers: []string{"ts"}}))
	reg := layers.New(root, nil, nil)
	require.NoError(t, routing.Sync(root, reg, "gitea"))
	require.FileExists(t, configgen.SitesFile(root, "gitea"))

	require.NoError(t, routing.Apply(root, reg, "gitea", func(s *exposure.State) { s.Set("ts", false) }))
	assert.NoFileExists(t, configgen.SitesFile(root, "gitea"))
	assert.NoFileExists(t, exposure.Path(root, "gitea"), "a service on no layer has no state file")
}

// A service with no ports and no routes cannot be routed, and says so instead
// of writing an empty block.
func TestSync_HeadlessServiceIsAnError(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "postgres", map[string]string{"config.yaml": "vars: {}\n"})
	require.NoError(t, exposure.Save(root, "postgres", exposure.State{Layers: []string{"ts"}}))
	err := routing.Sync(root, layers.New(root, nil, nil), "postgres")
	assert.ErrorContains(t, err, "no ports defined")
}

// fakeDaemon is a layer with daemon-side state, recording what routing asks.
type fakeDaemon struct {
	*tailscale.Layer
	calls []string
}

func (f *fakeDaemon) Name() string { return "fake" }
func (f *fakeDaemon) Configure(svc, display string, _ []network.PortSelection) error {
	f.calls = append(f.calls, "configure "+svc+" "+display)
	return nil
}
func (f *fakeDaemon) Teardown(svc string) error {
	f.calls = append(f.calls, "teardown "+svc)
	return nil
}

// Configure passes the stored --name (or the service name) to the daemon;
// layers without one are skipped.
func TestConfigureTeardown_DriveTheDaemonSide(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	l := &fakeDaemon{Layer: tailscale.New(root, nil, nil)}
	st := exposure.State{Name: "git"}
	e, err := routing.Resolve(root, "gitea", st)
	require.NoError(t, err)

	require.NoError(t, routing.Configure(l, "gitea", st, e))
	require.NoError(t, routing.Configure(l, "gitea", exposure.State{}, e))
	require.NoError(t, routing.Teardown(l, "gitea"))
	assert.Equal(t, []string{"configure gitea git", "configure gitea gitea", "teardown gitea"}, l.calls)

	assert.NoError(t, routing.Configure(tailscale.New(root, nil, nil), "gitea", st, e))
}
