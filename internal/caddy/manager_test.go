package caddy

// White-box test — same package so we can access newForTest and the unexported
// reloadFn field to bypass Docker/Caddy without changing the production API.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
)

// noopReload is injected in every test to skip Docker exec.
func noopReload() error { return nil }

// writeSvc lays out an installed service with the given files.
func writeSvc(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "services", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	for f, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600))
	}
}

const routesBody = "handle /api/* {\n\treverse_proxy svc-api:8000\n}\n\nhandle {\n\treverse_proxy svc:80\n}\n"

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// ── ReloadService ─────────────────────────────────────────────────────────────

// A reload must refresh the layers that are on without switching on the ones
// that are off.
func TestReloadService_Routes_OnlyTouchesActiveLayers(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "appflowy", map[string]string{
		"config.yaml": "ports:\n  - 80\n", configgen.RoutesFileName: routesBody,
	})
	privatePath := configgen.GeneratedFilePath(root, "conf.d", "appflowy", "")
	cfPath := configgen.GeneratedFilePath(root, "conf.d-cf", "appflowy", "")
	require.NoError(t, configgen.WriteFile(root, "conf.d", "appflowy", "", "stale\n"))

	require.NoError(t, newForTest(root, noopReload).ReloadService("appflowy"))

	assert.Contains(t, read(t, privatePath), "reverse_proxy svc-api:8000", "active layer should be regenerated")
	assert.NoFileExists(t, cfPath, "reload must not enable a layer the user left off")
}

// Port-driven services — most of the catalog — have no symlink and no routes
// file. Reload used to fail on them with "no active routes".
func TestReloadService_Ports_RegeneratesAndKeepsName(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	priv := configgen.GeneratedFilePath(root, "conf.d", "gitea", "")
	cf := configgen.GeneratedFilePath(root, "conf.d-cf", "gitea", "")
	// Enabled earlier with --name git on both layers; the upstream port since changed.
	require.NoError(t, configgen.WriteFile(root, "conf.d", "gitea", "",
		"git.{$HOME_SUBDOMAIN}.{$DOMAIN} {\n    reverse_proxy gitea:2000\n}\n"))
	require.NoError(t, configgen.WriteFile(root, "conf.d-cf", "gitea", "",
		"http://git.{$DOMAIN} {\n    reverse_proxy gitea:2000\n}\n"))

	reloads := 0
	m := newForTest(root, func() error { reloads++; return nil })
	require.NoError(t, m.ReloadService("gitea"))

	assert.Contains(t, read(t, priv), "git.{$HOME_SUBDOMAIN}.{$DOMAIN} {")
	assert.Contains(t, read(t, priv), "reverse_proxy gitea:3000")
	assert.Contains(t, read(t, cf), "http://git.{$DOMAIN} {")
	assert.Contains(t, read(t, cf), "reverse_proxy gitea:3000")
	assert.Equal(t, 1, reloads, "every layer is rewritten, then Caddy reloads once")
}

func TestReloadService_NothingActive(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	err := newForTest(root, noopReload).ReloadService("gitea")
	assert.ErrorContains(t, err, "no active routes",
		"reloading a service with no enabled layer should say so, not silently succeed")
	assert.NoFileExists(t, configgen.GeneratedFilePath(root, "conf.d", "gitea", ""))
}

// ── Rollback ──────────────────────────────────────────────────────────────────

// A config Caddy rejects must not stay on disk: Caddy would refuse to start at
// its next restart, taking every route down.
func TestReloadService_RollsBackWhenValidationFails(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	require.NoError(t, configgen.WriteFile(root, "conf.d", "gitea", "", "old\n"))
	other := filepath.Join(configgen.ConfigDir(root, "conf.d"), "other.conf")
	require.NoError(t, os.WriteFile(other, []byte("# kept\n"), 0o600))

	m := newForTest(root, func() error { return fmt.Errorf("%w: bad", ErrInvalidConfig) })
	err := m.ReloadService("gitea")
	require.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "rolled back")

	assert.Equal(t, "old\n", read(t, configgen.GeneratedFilePath(root, "conf.d", "gitea", "")))
	assert.Equal(t, "# kept\n", read(t, other), "unrelated config is untouched")
}

// Only a validation failure rolls back; any other reload error (Caddy down)
// leaves the change in place for the next start.
func TestReloadOrRestore_KeepsChangeOnOtherErrors(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	require.NoError(t, configgen.WriteFile(root, "conf.d", "gitea", "", "old\n"))

	m := newForTest(root, func() error { return errors.New("caddy not running") })
	require.Error(t, m.ReloadService("gitea"))
	assert.Contains(t, read(t, configgen.GeneratedFilePath(root, "conf.d", "gitea", "")), "reverse_proxy gitea:3000")
}

// Snapshot/Restore is what cmd/enable uses around writes made by other hands.
func TestSnapshot_RestoreUndoesAddsChangesAndRemovals(t *testing.T) {
	repo := t.TempDir()
	dir := configgen.ConfigDir(repo, "conf.d-cf")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	changed := filepath.Join(dir, "changed.conf")
	removed := filepath.Join(dir, "removed.conf")
	require.NoError(t, os.WriteFile(changed, []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(removed, []byte("gone\n"), 0o600))

	snap, err := newForTest(repo, noopReload).Snapshot()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(changed, []byte("new\n"), 0o600))
	require.NoError(t, os.Remove(removed))
	require.NoError(t, os.MkdirAll(configgen.ConfigDir(repo, "conf.d-tor"), 0o750))
	added := filepath.Join(configgen.ConfigDir(repo, "conf.d-tor"), "added.conf")
	require.NoError(t, os.WriteFile(added, []byte("x\n"), 0o600))

	require.NoError(t, snap.Restore())

	assert.Equal(t, "old\n", read(t, changed))
	assert.Equal(t, "gone\n", read(t, removed))
	assert.NoFileExists(t, added)
}
