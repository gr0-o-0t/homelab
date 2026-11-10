package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/desired"
)

func putFile(t *testing.T, root, svc, rel, content string) {
	t.Helper()
	rootCmd.SetIn(strings.NewReader(content))
	t.Cleanup(func() { rootCmd.SetIn(nil) })
	runCLI(t, root, "files", "put", svc, rel)
}

// An edited config.yaml that adds a shared database is rolled out the way
// `homelab up` does: the database is provisioned before the deploy.
func TestFilesPut_ConfigEditProvisionsBeforeDeploy(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)
	recordService(root, "alpha", desired.Running)

	var provisioned []string
	old := provisionDBs
	provisionDBs = func(_ context.Context, r, name string) error {
		cfg, err := config.Load(config.ServiceConfigFile(r, name))
		require.NoError(t, err)
		dbs, err := cfg.ServiceDatabases()
		require.NoError(t, err)
		require.Len(t, dbs, 1, "the edited config.yaml is what gets provisioned")
		provisioned = append(provisioned, name)
		assert.Zero(t, calls(f, "ServiceCreate"), "provisioned before the deploy")
		return nil
	}
	t.Cleanup(func() { provisionDBs = old })

	putFile(t, root, "alpha", "config.yaml",
		"databases:\n  - postgres:\n      database: alpha\n      user: alpha\n      env:\n        host: DB_HOST\n")

	assert.Equal(t, []string{"alpha"}, provisioned)
	assert.NotEmpty(t, alphaServices(t, f), "deployed after provisioning")
	assert.Equal(t, desired.Running, loadDesired(t, root).Services["alpha"])
}

// A stopped service's file is saved but the service is not started.
func TestFilesPut_StoppedServiceNotStarted(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)
	recordService(root, "alpha", desired.Stopped)

	edited := "services:\n  web:\n    image: nginx:1\n    hostname: edited\n"
	putFile(t, root, "alpha", "docker-compose.yml", edited)

	b, err := os.ReadFile(filepath.Join(root, "services", "alpha", "docker-compose.yml"))
	require.NoError(t, err)
	assert.Equal(t, edited, string(b), "saved")
	assert.Zero(t, calls(f, "ServiceCreate"), "not started")
	assert.Equal(t, desired.Stopped, loadDesired(t, root).Services["alpha"])
}
