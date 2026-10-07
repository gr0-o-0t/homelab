package service_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/service"
)

// ── repo fixture helpers ──────────────────────────────────────────────────────

// newRepo creates a minimal homelab repo skeleton in a temp dir.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{
		"caddy/conf.d",
		"caddy/conf.d-cf",
		"caddy/conf.d-tor",
		"caddy/conf.d-i2p",
		"caddy/conf.d-ygg",
		"services",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	return dir
}

// addService creates a minimal service directory. Optional files can be added
// by passing pairs of (relPath, content) via extras.
func addService(t *testing.T, repo, name string, extras ...string) string {
	t.Helper()
	svcDir := filepath.Join(repo, "services", name)
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	// Always write docker-compose.yml so validateService passes.
	write(t, filepath.Join(svcDir, "docker-compose.yml"), "services: {}\n")
	for i := 0; i+1 < len(extras); i += 2 {
		write(t, filepath.Join(svcDir, extras[i]), extras[i+1])
	}
	return svcDir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// expose records name as exposed on a layer, the way enable does.
func expose(t *testing.T, repo, name, layer string) {
	t.Helper()
	require.NoError(t, exposure.Update(repo, name, func(s *exposure.State) { s.Set(layer, true) }))
}

func enablePrivate(t *testing.T, repo, name string) { expose(t, repo, name, "ts") }
func enablePublic(t *testing.T, repo, name string)  { expose(t, repo, name, "cf") }
func enableTor(t *testing.T, repo, name string)     { expose(t, repo, name, "tor") }
func enableI2P(t *testing.T, repo, name string)     { expose(t, repo, name, "i2p") }
func enableYgg(t *testing.T, repo, name string)     { expose(t, repo, name, "ygg") }

// ── Discover ──────────────────────────────────────────────────────────────────

func TestDiscover_EmptyServicesDir(t *testing.T) {
	repo := newRepo(t)
	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	assert.Empty(t, svcs)
}

func TestDiscover_MissingServicesDir(t *testing.T) {
	dir := t.TempDir() // no services/ subdirectory
	svcs, err := service.Discover(dir)
	require.NoError(t, err)
	assert.Nil(t, svcs)
}

func TestDiscover_BasicService(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)

	svc := svcs[0]
	assert.Equal(t, "myapp", svc.Name)
	assert.Equal(t, filepath.Join(repo, "services", "myapp"), svc.Dir)
	assert.False(t, svc.On("ts"))
	assert.False(t, svc.On("cf"))
}

func TestDiscover_PrivateEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enablePrivate(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].On("ts"), "ts in exposure.yaml → On(ts)")
	assert.False(t, svcs[0].On("cf"))
}

func TestDiscover_PublicEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enablePublic(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.False(t, svcs[0].On("ts"))
	assert.True(t, svcs[0].On("cf"), "regular file in conf.d-cf → PublicEnabled=true")
}

func TestDiscover_BothEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enablePrivate(t, repo, "myapp")
	enablePublic(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].On("ts"))
	assert.True(t, svcs[0].On("cf"))
}

func TestDiscover_MultipleServices_SortedAlphabetically(t *testing.T) {
	repo := newRepo(t)
	for _, name := range []string{"zebra", "alpha", "middle"} {
		addService(t, repo, name)
	}

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 3)

	names := []string{svcs[0].Name, svcs[1].Name, svcs[2].Name}
	assert.Equal(t, []string{"alpha", "middle", "zebra"}, names,
		"Discover must return services sorted alphabetically")
}

func TestDiscover_SkipsNonDirectories(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "realservice")
	// Write a plain file in services/ — should be ignored
	write(t, filepath.Join(repo, "services", "not-a-dir.txt"), "oops\n")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.Equal(t, "realservice", svcs[0].Name)
}

// Exposure is the state file, not whatever Caddy files exist: a legacy
// per-layer file (awaiting migration) or a stray sites file does not count.
func TestDiscover_FilesWithoutStateDoNotCount(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	write(t, filepath.Join(repo, "caddy", "conf.d", "myapp.conf"), "# legacy\n")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "caddy", "sites"), 0o755))
	write(t, filepath.Join(repo, "caddy", "sites", "myapp.conf"), "# stray\n")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.Empty(t, svcs[0].ActiveLayers())
}

// Layers come back in registry order, whatever order the file lists them in.
func TestDiscover_LayersInRegistryOrder(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	require.NoError(t, exposure.Save(repo, "myapp", exposure.State{Layers: []string{"ygg", "cf", "ts", "bogus"}}))

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	assert.Equal(t, []string{"ts", "cf", "ygg"}, svcs[0].ActiveLayers())
}

// ── Network extension layer detection ─────────────────────────────────────────

func TestDiscover_TorLayer(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enableTor(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].On("tor"), "tor in exposure.yaml")
	assert.False(t, svcs[0].On("i2p"))
	assert.False(t, svcs[0].On("ygg"))
}

func TestDiscover_I2PLayer(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enableI2P(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].On("i2p"), "i2p in exposure.yaml")
	assert.False(t, svcs[0].On("tor"))
	assert.False(t, svcs[0].On("ygg"))
}

func TestDiscover_YggLayer(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enableYgg(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].On("ygg"), "ygg in exposure.yaml")
	assert.False(t, svcs[0].On("tor"))
	assert.False(t, svcs[0].On("i2p"))
}

func TestDiscover_AllExtensionLayers(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enableTor(t, repo, "myapp")
	enableI2P(t, repo, "myapp")
	enableYgg(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].On("tor"))
	assert.True(t, svcs[0].On("i2p"))
	assert.True(t, svcs[0].On("ygg"))
}

func TestDiscover_ExtensionLayerWithoutService(t *testing.T) {
	repo := newRepo(t)
	// A config file in conf.d-tor/ for a service that doesn't exist.
	write(t, filepath.Join(repo, "caddy", "conf.d-tor", "nonexistent.conf"), "# tor config\n")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	assert.Empty(t, svcs, "no service dir → no discovery, even with orphaned config")
}
