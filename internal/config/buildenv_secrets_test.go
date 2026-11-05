package config_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
)

// BuildEnvWithSecrets reports every env key whose value came from (or embeds
// a value from) the secrets manager, so the Swarm translator can refuse a
// spec that would show one.
func TestBuildEnvWithSecrets_ReportsSecretSourcedKeys(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
vars:
  DOMAIN:
    value: example.com
  SHADOWED:
    value: root-var
secrets:
  CF_TOKEN:
    required: true
  SHADOWED_SECRET:
    required: true
`)
	writeFile(t, dir, "services/gitea/config.yaml", `
vars:
  SHADOWED_SECRET:
    value: plain
secrets:
  APP_KEY:
    generate: hex32
databases:
  - postgres:
      env:
        host: DB_HOST
        user: DB_USER
        password: DB_PASS
        dsn: DB_URL
`)
	sm := memSecrets(t, map[[2]string]string{
		{"", "CF_TOKEN"}:                    "cftoken",
		{"", "SHADOWED_SECRET"}:             "rootsecret",
		{"gitea", "APP_KEY"}:                "appkey",
		{"", config.DBPasswordKey("gitea")}: "dbpw",
	})

	env, keys, err := config.BuildEnvWithSecrets(rootPath, dir, "gitea", sm)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"CF_TOKEN", "APP_KEY", "DB_PASS", "DB_URL"}, keys)
	assert.Equal(t, "plain", env["SHADOWED_SECRET"], "a service var overrides a root secret and is no longer secret")
	assert.Contains(t, env["DB_URL"], "dbpw")

	plain, err := config.BuildEnv(rootPath, dir, "gitea", sm)
	require.NoError(t, err)
	assert.Equal(t, env, plain, "BuildEnv is unchanged")
}
