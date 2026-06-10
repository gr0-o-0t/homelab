package caddy

// White-box test — same package so we can access newForTest and the unexported
// reloadFn field to bypass Docker/Caddy without changing the production API.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── repo fixture helpers ──────────────────────────────────────────────────────

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

func addService(t *testing.T, repo, name string) {
	t.Helper()
	svcDir := filepath.Join(repo, "services", name)
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
}

func writeCaddyConf(t *testing.T, repo, name string) {
	t.Helper()
	path := filepath.Join(repo, "services", name, "caddy.conf")
	require.NoError(t, os.WriteFile(path, []byte("# caddy.conf\n"), 0o644))
}

func writePubCaddyConf(t *testing.T, repo, name string) {
	t.Helper()
	path := filepath.Join(repo, "services", name, "caddy.cf.conf")
	require.NoError(t, os.WriteFile(path, []byte("# caddy.cf.conf\n"), 0o644))
}

// noopReload is injected in every test to skip Docker exec.
func noopReload() error { return nil }

// mgr returns a test Manager with a no-op reload.
func mgr(t *testing.T, repo string) *Manager {
	t.Helper()
	return newForTest(repo, noopReload)
}

// ── Enable ────────────────────────────────────────────────────────────────────

func TestEnable_CreatesSymlink(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")

	require.NoError(t, mgr(t, repo).Enable("myapp"))

	dest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	fi, err := os.Lstat(dest)
	require.NoError(t, err)
	assert.True(t, fi.Mode()&os.ModeSymlink != 0, "should be a symlink")
}

func TestEnable_SymlinkIsRelative(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")

	require.NoError(t, mgr(t, repo).Enable("myapp"))

	dest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	target, err := os.Readlink(dest)
	require.NoError(t, err)
	assert.False(t, filepath.IsAbs(target),
		"symlink target must be relative for portability, got: %s", target)
	assert.Equal(t, filepath.Join("..", "..", "services", "myapp", "caddy.conf"), target)
}

func TestEnable_ErrorWhenNoCaddyConf(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp") // no caddy.conf

	err := mgr(t, repo).Enable("myapp")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "caddy.conf")
}

func TestEnable_ReplacesStaleSymlink(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")

	m := mgr(t, repo)
	require.NoError(t, m.Enable("myapp")) // first enable
	require.NoError(t, m.Enable("myapp")) // second enable should not error

	dest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	fi, err := os.Lstat(dest)
	require.NoError(t, err)
	assert.True(t, fi.Mode()&os.ModeSymlink != 0)
}

func TestEnable_PropagatesReloadError(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")

	reloadErr := errors.New("caddy config invalid")
	m := newForTest(repo, func() error { return reloadErr })

	err := m.Enable("myapp")
	assert.ErrorIs(t, err, reloadErr)
}

// ── Disable ───────────────────────────────────────────────────────────────────

func TestDisable_RemovesSymlink(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")
	m := mgr(t, repo)
	require.NoError(t, m.Enable("myapp"))

	require.NoError(t, m.Disable("myapp"))

	dest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	_, err := os.Lstat(dest)
	assert.True(t, os.IsNotExist(err), "symlink should be gone after Disable")
}

func TestDisable_ErrorWhenNotEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")

	err := mgr(t, repo).Disable("myapp")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no active private route")
}

func TestDisable_RefusesRegularFile(t *testing.T) {
	repo := newRepo(t)
	// Write a plain file where the symlink would go — should refuse to remove it.
	dest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	require.NoError(t, os.WriteFile(dest, []byte("# real file"), 0o644))

	err := mgr(t, repo).Disable("myapp")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a symlink")
}

// ── IsEnabled ─────────────────────────────────────────────────────────────────

func TestIsEnabled_TrueAfterEnable(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")
	m := mgr(t, repo)
	require.NoError(t, m.Enable("myapp"))

	ok, err := m.IsEnabled("myapp")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestIsEnabled_FalseWhenNotEnabled(t *testing.T) {
	repo := newRepo(t)
	ok, err := mgr(t, repo).IsEnabled("myapp")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestIsEnabled_FalseAfterDisable(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")
	m := mgr(t, repo)
	require.NoError(t, m.Enable("myapp"))
	require.NoError(t, m.Disable("myapp"))

	ok, err := m.IsEnabled("myapp")
	require.NoError(t, err)
	assert.False(t, ok)
}

// ── EnablePublic ──────────────────────────────────────────────────────────────

func TestEnablePublic_CreatesSymlink(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writePubCaddyConf(t, repo, "myapp")

	require.NoError(t, mgr(t, repo).EnablePublic("myapp"))

	dest := filepath.Join(repo, "caddy", "conf.d-cf", "myapp.conf")
	fi, err := os.Lstat(dest)
	require.NoError(t, err)
	assert.True(t, fi.Mode()&os.ModeSymlink != 0)
}

func TestEnablePublic_SymlinkIsRelative(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writePubCaddyConf(t, repo, "myapp")
	require.NoError(t, mgr(t, repo).EnablePublic("myapp"))

	dest := filepath.Join(repo, "caddy", "conf.d-cf", "myapp.conf")
	target, err := os.Readlink(dest)
	require.NoError(t, err)
	assert.False(t, filepath.IsAbs(target))
	assert.Equal(t, filepath.Join("..", "..", "services", "myapp", "caddy.cf.conf"), target)
}

func TestEnablePublic_ErrorWhenNoPublicCaddyConf(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp") // no caddy.cf.conf

	err := mgr(t, repo).EnablePublic("myapp")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "caddy.cf.conf")
}

// ── DisablePublic ─────────────────────────────────────────────────────────────

func TestDisablePublic_RemovesSymlink(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writePubCaddyConf(t, repo, "myapp")
	m := mgr(t, repo)
	require.NoError(t, m.EnablePublic("myapp"))
	require.NoError(t, m.DisablePublic("myapp"))

	dest := filepath.Join(repo, "caddy", "conf.d-cf", "myapp.conf")
	_, err := os.Lstat(dest)
	assert.True(t, os.IsNotExist(err))
}

func TestDisablePublic_ErrorWhenNotEnabled(t *testing.T) {
	repo := newRepo(t)
	err := mgr(t, repo).DisablePublic("myapp")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no active public route")
}

// ── IsPublicEnabled ───────────────────────────────────────────────────────────

func TestIsPublicEnabled_TrueAfterEnable(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writePubCaddyConf(t, repo, "myapp")
	m := mgr(t, repo)
	require.NoError(t, m.EnablePublic("myapp"))

	ok, err := m.IsPublicEnabled("myapp")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestIsPublicEnabled_FalseWhenNotEnabled(t *testing.T) {
	repo := newRepo(t)
	ok, err := mgr(t, repo).IsPublicEnabled("myapp")
	require.NoError(t, err)
	assert.False(t, ok)
}

// ── DisableBoth ───────────────────────────────────────────────────────────────

func TestDisableBoth_RemovesBothSymlinks(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")
	writePubCaddyConf(t, repo, "myapp")
	m := mgr(t, repo)
	require.NoError(t, m.Enable("myapp"))
	require.NoError(t, m.EnablePublic("myapp"))

	require.NoError(t, m.DisableBoth("myapp"))

	privDest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	pubDest := filepath.Join(repo, "caddy", "conf.d-cf", "myapp.conf")
	_, errPriv := os.Lstat(privDest)
	_, errPub := os.Lstat(pubDest)
	assert.True(t, os.IsNotExist(errPriv), "private symlink should be removed")
	assert.True(t, os.IsNotExist(errPub), "public symlink should be removed")
}

func TestDisableBoth_IdempotentWhenNeitherEnabled(t *testing.T) {
	repo := newRepo(t)
	// Neither symlink exists — DisableBoth should still succeed.
	assert.NoError(t, mgr(t, repo).DisableBoth("myapp"))
}

func TestDisableBoth_PartialState_OnlyPrivateEnabled(t *testing.T) {
	repo := newRepo(t)
	addService(t, repo, "myapp")
	writeCaddyConf(t, repo, "myapp")
	m := mgr(t, repo)
	require.NoError(t, m.Enable("myapp"))
	// Public is NOT enabled.

	require.NoError(t, m.DisableBoth("myapp"))

	privDest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	_, err := os.Lstat(privDest)
	assert.True(t, os.IsNotExist(err))
}

func TestDisableBoth_RemovesRegularFile(t *testing.T) {
	repo := newRepo(t)
	// Simulate a generated config file (regular file) where the symlink should be.
	dest := filepath.Join(repo, "caddy", "conf.d", "myapp.conf")
	require.NoError(t, os.WriteFile(dest, []byte("# generated config"), 0o644))

	// Should succeed — generated files are removed by configgen.RemoveFile path.
	err := mgr(t, repo).DisableBoth("myapp")
	require.NoError(t, err)

	// Verify the file was removed.
	_, err = os.Lstat(dest)
	assert.True(t, os.IsNotExist(err))
}
