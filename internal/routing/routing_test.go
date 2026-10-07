package routing_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/tailscale"
	"github.com/groot/homelab/internal/routing"
)

func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "caddy", "conf.d"), 0o750))
	return root
}

func writeSvc(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "services", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	for f, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte(content), 0o600))
	}
}

// A service that declares ports gets a generated block.
func TestEnable_GeneratesFromDeclaredPorts(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - web:3000\n"})

	require.NoError(t, routing.Enable(root, tailscale.New(root, nil, nil), "gitea", "", nil))

	data, err := os.ReadFile(filepath.Join(root, "caddy", "conf.d", "gitea.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "reverse_proxy gitea:3000")
	assert.Contains(t, string(data), "import wildcard_tls")
}

// A service with no ports and no routes cannot be routed, and
// says so instead of writing an empty block.
func TestEnable_HeadlessServiceIsAnError(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "postgres", map[string]string{"config.yaml": "vars: {}\n"})

	err := routing.Enable(root, tailscale.New(root, nil, nil), "postgres", "", nil)
	assert.ErrorContains(t, err, "no ports defined")
}

// Disable must clear the route whichever way it was written, and must not
// complain when there is nothing left — `delete` calls it for that reason.
func TestDisable_RemovesGeneratedRoute(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - web:3000\n"})
	require.NoError(t, routing.Enable(root, tailscale.New(root, nil, nil), "gitea", "", nil))

	require.NoError(t, routing.Disable(root, tailscale.New(root, nil, nil), "gitea"))
	_, err := os.Stat(filepath.Join(root, "caddy", "conf.d", "gitea.conf"))
	assert.True(t, os.IsNotExist(err))
}

func TestEnable_DisplayNameOverridesSubdomain(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "vaultwarden", map[string]string{"config.yaml": "ports:\n  - 80\n"})

	require.NoError(t, routing.Enable(root, tailscale.New(root, nil, nil), "vaultwarden", "vault", nil))
	data, err := os.ReadFile(filepath.Join(root, "caddy", "conf.d", "vaultwarden.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "vault.{$HOME_SUBDOMAIN}")
	assert.Contains(t, string(data), "reverse_proxy vaultwarden:80")
}

// fakeDaemon is a layer with daemon-side state, recording what routing asks.
type fakeDaemon struct {
	*tailscale.Layer
	calls []string
}

func (f *fakeDaemon) Name() string    { return "fake" }
func (f *fakeDaemon) Flag() string    { return "fake" }
func (f *fakeDaemon) ConfDir() string { return "conf.d-fake" }
func (f *fakeDaemon) Configure(svc, display string, ports []network.PortSelection) error {
	f.calls = append(f.calls, "configure "+svc+" "+display)
	return nil
}
func (f *fakeDaemon) Teardown(svc string) error {
	f.calls = append(f.calls, "teardown "+svc)
	return nil
}

// Every layer goes through the same path: the daemon side, then the blocks;
// on disable the blocks, then the daemon side.
func TestEnableDisable_DriveTheDaemonSide(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - web:3000\n"})
	l := &fakeDaemon{Layer: tailscale.New(root, nil, nil)}

	require.NoError(t, routing.Enable(root, l, "gitea", "", nil))
	assert.FileExists(t, filepath.Join(root, "caddy", "conf.d-fake", "gitea.conf"))

	require.NoError(t, routing.Disable(root, l, "gitea"))
	assert.NoFileExists(t, filepath.Join(root, "caddy", "conf.d-fake", "gitea.conf"))
	assert.Equal(t, []string{"configure gitea gitea", "teardown gitea"}, l.calls)
}
