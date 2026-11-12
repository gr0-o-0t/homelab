package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/db"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

// Deleting a service frees its redis numbers, a file in the config dir,
// even when Docker is unreachable; only the garage key needs Docker, and
// its failure is reported on its own.
func TestReleaseDBDependencies_RedisReleasedWithoutDocker(t *testing.T) {
	root := t.TempDir()
	svcDir := filepath.Join(root, "services", "app")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(svcDir, "config.yaml"), []byte(`databases:
  - redis:
      env:
        db: REDIS_DB
  - s3:
      env:
        bucket: S3_BUCKET
`), 0o644))
	_, _, err := config.AllocateRedisDB(root, "app", "")
	require.NoError(t, err)

	oldAPI, oldSecrets := newSwarmAPI, openSecrets
	t.Cleanup(func() { newSwarmAPI, openSecrets = oldAPI, oldSecrets })
	newSwarmAPI = func() (swarm.API, error) { return nil, errors.New("docker unreachable") }
	sm := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	openSecrets = func() (*secrets.Manager, error) { return sm, nil }

	errs := releaseDBDependencies(context.Background(), root, "app")

	m, err := config.LoadRedisDBs(root)
	require.NoError(t, err)
	assert.NotContains(t, m, config.RedisDBKey("app", ""), "redis number must be released without Docker")
	require.Len(t, errs, 1, "only the garage key fails")
	assert.True(t, strings.Contains(errs[0].Error(), "garage key for app not deleted") &&
		strings.Contains(errs[0].Error(), "docker unreachable"), "got %v", errs[0])
}

// Deleting a consumer drops its read-only role on another service's
// database; its own postgres database and role are kept, as before.
func TestDelete_DropsReadOnlyRole(t *testing.T) {
	root := t.TempDir()
	svcDir := filepath.Join(root, "services", "grafana")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(svcDir, "config.yaml"), []byte(`databases:
  - postgres:
      database: grafana
      user: grafana
  - postgres:
      database: taash
      user: taash_ro
      readonly: true
`), 0o644))

	oldAPI, oldSecrets, oldDeprov := newSwarmAPI, openSecrets, deprovisionDB
	t.Cleanup(func() { newSwarmAPI, openSecrets, deprovisionDB = oldAPI, oldSecrets, oldDeprov })
	newSwarmAPI = func() (swarm.API, error) { return swarmtest.New(), nil }
	sm := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	openSecrets = func() (*secrets.Manager, error) { return sm, nil }
	var dropped []config.ServiceDBDecl
	deprovisionDB = func(_ *db.Provisioner, _ context.Context, dbType config.DBType, svc string, decl config.ServiceDBDecl) error {
		assert.Equal(t, config.DBPostgres, dbType)
		assert.Equal(t, "grafana", svc)
		dropped = append(dropped, decl)
		return nil
	}

	errs := releaseDBDependencies(context.Background(), root, "grafana")
	require.Empty(t, errs)
	require.Len(t, dropped, 1, "only the read-only declaration is released")
	assert.Equal(t, "taash_ro", dropped[0].User)
	assert.True(t, dropped[0].ReadOnly)
}

// writeROConsumer installs svc with a read-only declaration of user on taash.
func writeROConsumer(t *testing.T, root, svc, user string) {
	t.Helper()
	dir := filepath.Join(root, "services", svc)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`databases:
  - postgres:
      database: taash
      user: `+user+`
      readonly: true
`), 0o644))
}

// Two consumers declaring one read-only user would share a role (and reset
// each other's password): the second is refused before anything runs.
func TestEnsureDBDependencies_RefusesReadOnlyUserOfAnotherService(t *testing.T) {
	root := t.TempDir()
	writeROConsumer(t, root, "grafana", "taash_ro")
	writeROConsumer(t, root, "metabase", "taash_ro")

	oldAPI, oldSecrets := newSwarmAPI, openSecrets
	t.Cleanup(func() { newSwarmAPI, openSecrets = oldAPI, oldSecrets })
	newSwarmAPI = func() (swarm.API, error) { t.Fatal("docker reached"); return nil, nil }

	err := ensureDBDependencies(context.Background(), root, "metabase")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `read-only user "taash_ro" is already declared by grafana`)
}

// Deleting one of two consumers that (from before the refusal) declare the
// same read-only user keeps the role for the other.
func TestDelete_KeepsReadOnlyRoleAnotherServiceDeclares(t *testing.T) {
	root := t.TempDir()
	writeROConsumer(t, root, "grafana", "taash_ro")
	writeROConsumer(t, root, "metabase", "taash_ro")

	oldAPI, oldSecrets, oldDeprov := newSwarmAPI, openSecrets, deprovisionDB
	t.Cleanup(func() { newSwarmAPI, openSecrets, deprovisionDB = oldAPI, oldSecrets, oldDeprov })
	newSwarmAPI = func() (swarm.API, error) { return swarmtest.New(), nil }
	sm := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	openSecrets = func() (*secrets.Manager, error) { return sm, nil }
	deprovisionDB = func(*db.Provisioner, context.Context, config.DBType, string, config.ServiceDBDecl) error {
		t.Error("the role another service declares was dropped")
		return nil
	}

	require.Empty(t, releaseDBDependencies(context.Background(), root, "metabase"))
}
