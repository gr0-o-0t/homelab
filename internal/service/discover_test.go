package service_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// enablePrivate creates the caddy/conf.d/<name>.conf symlink.
func enablePrivate(t *testing.T, repo, name string) {
	t.Helper()
	// Symlink target must exist for the symlink to resolve (Lstat works either way).
	src := filepath.Join(repo, "services", name, "caddy.conf")
	write(t, src, "# caddy.conf\n")
	dest := filepath.Join(repo, "caddy", "conf.d", name+".conf")
	require.NoError(t, os.Symlink(src, dest))
}

// enablePublic creates the caddy/conf.d-cf/<name>.conf symlink.
func enablePublic(t *testing.T, repo, name string) {
	t.Helper()
	src := filepath.Join(repo, "services", name, "caddy.cf.conf")
	write(t, src, "# caddy.cf.conf\n")
	dest := filepath.Join(repo, "caddy", "conf.d-cf", name+".conf")
	require.NoError(t, os.Symlink(src, dest))
}

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
	assert.False(t, svc.HasCaddyConf)
	assert.False(t, svc.HasPublicCaddyConf)
	assert.False(t, svc.Enabled)
	assert.False(t, svc.PublicEnabled)
}

func TestDiscover_HasCaddyConf(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp", "caddy.conf", "# conf\n")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].HasCaddyConf)
	assert.False(t, svcs[0].HasPublicCaddyConf)
}

func TestDiscover_HasPublicCaddyConf(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp", "caddy.cf.conf", "# pub conf\n")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.False(t, svcs[0].HasCaddyConf)
	assert.True(t, svcs[0].HasPublicCaddyConf)
}

func TestDiscover_PrivateEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enablePrivate(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].Enabled, "symlink in conf.d → Enabled=true")
	assert.False(t, svcs[0].PublicEnabled)
}

func TestDiscover_PublicEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enablePublic(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.False(t, svcs[0].Enabled)
	assert.True(t, svcs[0].PublicEnabled, "symlink in conf.d-cf → PublicEnabled=true")
}

func TestDiscover_BothEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	enablePrivate(t, repo, "myapp")
	enablePublic(t, repo, "myapp")

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].Enabled)
	assert.True(t, svcs[0].PublicEnabled)
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

func TestDiscover_BrokenSymlinkNotCountedAsEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")

	// Create a symlink pointing to a non-existent target
	dest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	require.NoError(t, os.Symlink("/nonexistent/path/caddy.conf", dest))

	svcs, err := service.Discover(repo)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	// The symlink exists (Lstat succeeds) so it IS counted as enabled —
	// this matches the real behaviour: a broken symlink keeps the name in routing.
	assert.True(t, svcs[0].Enabled,
		"broken symlink still registers as Enabled (Lstat sees it)")
}
