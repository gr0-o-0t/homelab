package assets_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/assets"
	"github.com/groot/homelab/internal/config"
)

// Bifrost reads its Postgres settings out of config.json via "env.VAR"
// references rather than from the environment directly, so the two files have
// to agree: every BIFROST_PG_* name referenced in bifrost-config.json must be
// declared in the service's databases block, or the gateway silently starts
// with an unresolved connection and can't reach homelab-postgres.
func TestBifrost_ConfigJSONEnvRefsAreDeclared(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	data, err := assets.CatalogFS.ReadFile("services/bifrost/config.yaml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, data, 0o600))

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err, "bifrost config.yaml must parse against the service schema")

	dbs, err := cfg.ServiceDatabases()
	require.NoError(t, err, "bifrost databases block must decode")

	declared := make(map[string]bool)
	for _, entry := range dbs {
		for _, target := range entry.Env {
			declared[target] = true
		}
	}

	confJSON, err := assets.CatalogFS.ReadFile("services/bifrost/bifrost-config.json")
	require.NoError(t, err)

	for _, name := range []string{
		"BIFROST_PG_HOST", "BIFROST_PG_PORT", "BIFROST_PG_USER",
		"BIFROST_PG_PASSWORD", "BIFROST_PG_DB",
	} {
		assert.Contains(t, string(confJSON), "env."+name,
			"bifrost-config.json should reference %s", name)
		assert.True(t, declared[name],
			"%s is referenced by bifrost-config.json but not declared in config.yaml databases.env", name)
	}
}
