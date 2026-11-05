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
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
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
