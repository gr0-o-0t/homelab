package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/cmd"
)

func TestServiceAdd_PrintsCatalog(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "add"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

func TestServiceAdd_InstallsService(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"add", "uptime-kuma",
	})

	err := rootCmd.Execute()
	require.NoError(t, err)

	svcDir := filepath.Join(tmp, "services", "uptime-kuma")
	_, err = os.Stat(svcDir)
	require.NoError(t, err, "service directory should be created")

	composePath := filepath.Join(svcDir, "docker-compose.yml")
	_, err = os.Stat(composePath)
	require.NoError(t, err, "docker-compose.yml should exist")

	// uptime-kuma routes through caddy.routes.conf, not a static caddy.conf:
	// it needs websocket headers, and the routes body is wrapped per layer
	// rather than duplicated into one file per layer.
	routes := filepath.Join(svcDir, "caddy.routes.conf")
	_, err = os.Stat(routes)
	require.NoError(t, err, "caddy.routes.conf should exist")
}

func TestServiceAdd_DuplicateFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"add", "uptime-kuma",
	})
	require.NoError(t, rootCmd.Execute())

	rootCmd2 := cmd.RootCmd()
	rootCmd2.SetArgs([]string{
		"--config-dir", tmp,
		"add", "uptime-kuma",
	})

	err := rootCmd2.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestServiceAdd_InvalidServiceFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"add", "nonexistent-service-xyz",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no catalog entry")
}
