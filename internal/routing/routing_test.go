package routing_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// A service that declares ports and ships no caddy.conf gets a generated
// block. The TUI could not do this before — it only ever tried the symlink —
// so the same key did different things there and on the CLI.
func TestEnablePrivate_GeneratesFromDeclaredPorts(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - web:3000\n"})

	require.NoError(t, routing.EnablePrivate(root, "gitea", "", nil, nil))

	data, err := os.ReadFile(filepath.Join(root, "caddy", "conf.d", "gitea.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "reverse_proxy gitea:3000")
	assert.Contains(t, string(data), "import wildcard_tls")
}

// A service with no ports, no routes and no caddy.conf cannot be routed, and
// says so instead of writing an empty block.
func TestEnablePrivate_HeadlessServiceIsAnError(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "postgres", map[string]string{"config.yaml": "vars: {}\n"})

	err := routing.EnablePrivate(root, "postgres", "", nil, nil)
	assert.ErrorContains(t, err, "no ports defined")
}

// Disable must clear the route whichever way it was written, and must not
// complain when there is nothing left — `delete` calls it for that reason.
func TestDisablePrivate_RemovesGeneratedRoute(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - web:3000\n"})
	require.NoError(t, routing.EnablePrivate(root, "gitea", "", nil, nil))

	require.NoError(t, routing.DisablePrivate(root, "gitea", nil))
	_, err := os.Stat(filepath.Join(root, "caddy", "conf.d", "gitea.conf"))
	assert.True(t, os.IsNotExist(err))
}

func TestEnablePrivate_DisplayNameOverridesSubdomain(t *testing.T) {
	root := repo(t)
	writeSvc(t, root, "vaultwarden", map[string]string{"config.yaml": "ports:\n  - 80\n"})

	require.NoError(t, routing.EnablePrivate(root, "vaultwarden", "vault", nil, nil))
	data, err := os.ReadFile(filepath.Join(root, "caddy", "conf.d", "vaultwarden.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "vault.{$HOME_SUBDOMAIN}")
	assert.Contains(t, string(data), "reverse_proxy vaultwarden:80")
}
