package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
)

// Migration waits for a core that serves caddy/sites: before `homelab update`
// installs it, moving routes there would stop them being served.
func TestSitesReady_FollowsInstalledCore(t *testing.T) {
	dir := t.TempDir()
	assert.ErrorIs(t, sitesReady(dir), errCoreTooOld)

	require.NoError(t, installAssets(dir))
	assert.DirExists(t, configgen.SitesDir(dir))
	assert.NoError(t, sitesReady(dir), "no Docker in tests, so only the installed files are checked")
}

// The lazy migration run before commands moves a legacy route once the core
// is ready, and leaves an un-updated install alone.
func TestEnsureMigrated(t *testing.T) {
	dir := t.TempDir()
	writeSvc(t, dir, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	legacy := filepath.Join(dir, "caddy", "conf.d", "gitea.conf")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o750))
	require.NoError(t, os.WriteFile(legacy,
		[]byte("gitea.{$HOME_SUBDOMAIN}.{$DOMAIN} {\n\timport wildcard_tls\n\treverse_proxy gitea:3000\n}\n"), 0o600))

	ensureMigrated(dir)
	assert.FileExists(t, legacy, "core predates caddy/sites: nothing moves")
	assert.Error(t, requireMigrated(dir, "gitea"))

	require.NoError(t, installAssets(dir))
	ensureMigrated(dir)
	assert.NoFileExists(t, legacy)
	assert.FileExists(t, configgen.SitesFile(dir, "gitea"))
	st, err := exposure.Load(dir, "gitea")
	require.NoError(t, err)
	assert.Equal(t, []string{"ts"}, st.Layers)
	assert.NoError(t, requireMigrated(dir, "gitea"))
}
