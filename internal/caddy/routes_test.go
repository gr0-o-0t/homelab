package caddy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
)

// writeRoutesSvc lays out an installed service that ships caddy.routes.conf and
// therefore has no caddy.conf / caddy.cf.conf to symlink.
func writeRoutesSvc(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, "services", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("ports:\n  - 80\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, configgen.RoutesFileName),
		[]byte("handle /api/* {\n\treverse_proxy svc-api:8000\n}\n\nhandle {\n\treverse_proxy svc:80\n}\n"), 0o600))
}

// The TUI calls this package directly rather than going through
// `homelab enable`, so a routes-driven service has to work here too: the
// symlink helpers would otherwise fail with "no caddy.conf found".
func TestManager_RoutesService_EnableDisableRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeRoutesSvc(t, root, "appflowy")
	m := newForTest(root, func() error { return nil })

	for _, tc := range []struct {
		layer   string
		ext     string
		enable  func(string) error
		disable func(string) error
		active  func(string) (bool, error)
	}{
		{"private", "private", m.Enable, m.Disable, m.IsEnabled},
		{"public", "cf", m.EnablePublic, m.DisablePublic, m.IsPublicEnabled},
	} {
		t.Run(tc.layer, func(t *testing.T) {
			on, err := tc.active("appflowy")
			require.NoError(t, err)
			assert.False(t, on, "should start disabled")

			require.NoError(t, tc.enable("appflowy"), "enabling a routes service must not need caddy.conf")

			path := configgen.GeneratedFilePath(root, tc.ext, "appflowy", "")
			body, err := os.ReadFile(path)
			require.NoError(t, err, "generated config should exist at %s", path)
			assert.Contains(t, string(body), "reverse_proxy svc-api:8000",
				"the %s layer must carry every route, not just the catch-all", tc.layer)

			on, err = tc.active("appflowy")
			require.NoError(t, err)
			assert.True(t, on, "generated config is a regular file, not a symlink — must still read as enabled")

			require.NoError(t, tc.disable("appflowy"))
			assert.NoFileExists(t, path)

			on, err = tc.active("appflowy")
			require.NoError(t, err)
			assert.False(t, on)
		})
	}
}

// A reload must refresh the layers that are on without switching on the ones
// that are off.
func TestManager_RoutesService_ReloadOnlyTouchesActiveLayers(t *testing.T) {
	root := t.TempDir()
	writeRoutesSvc(t, root, "appflowy")
	m := newForTest(root, func() error { return nil })

	require.NoError(t, m.Enable("appflowy"))

	privatePath := configgen.GeneratedFilePath(root, "private", "appflowy", "")
	cfPath := configgen.GeneratedFilePath(root, "cf", "appflowy", "")
	require.NoError(t, os.WriteFile(privatePath, []byte("stale\n"), 0o600))

	require.NoError(t, m.ReloadService("appflowy"))

	body, err := os.ReadFile(privatePath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "reverse_proxy svc-api:8000", "active layer should be regenerated")
	assert.NoFileExists(t, cfPath, "reload must not enable a layer the user left off")
}

func TestManager_RoutesService_ReloadWithNothingActive(t *testing.T) {
	root := t.TempDir()
	writeRoutesSvc(t, root, "appflowy")
	m := newForTest(root, func() error { return nil })

	err := m.ReloadService("appflowy")
	assert.ErrorContains(t, err, "no active routes",
		"reloading a service with no enabled layer should say so, not silently succeed")
}
