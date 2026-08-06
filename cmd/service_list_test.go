package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/cmd"
)

func TestServiceList_Empty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"--no-color",
	})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

func TestServiceList_WithServices(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	err := os.MkdirAll(filepath.Join(tmp, "services", "test-svc"), 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmp, "services", "test-svc", "docker-compose.yml"), []byte("services: {}"), 0o644)
	require.NoError(t, err)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"--no-color",
	})

	err = rootCmd.Execute()
	assert.NoError(t, err)
}

func TestServiceList_JsonOutput(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	err := os.MkdirAll(filepath.Join(tmp, "services", "test-svc"), 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmp, "services", "test-svc", "docker-compose.yml"), []byte("services: {}"), 0o644)
	require.NoError(t, err)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"--json",
	})

	err = rootCmd.Execute()
	assert.NoError(t, err)
}

func TestServiceList_UnknownConfigDir(t *testing.T) {
	tmp := t.TempDir()
	nonexistent := filepath.Join(tmp, "nonexistent-homelab-config")

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", nonexistent,
	})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}
