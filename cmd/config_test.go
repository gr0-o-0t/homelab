package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/cmd"
)

func TestConfigDir_ExplicitFlag(t *testing.T) {
	tmp := t.TempDir()
	customDir := filepath.Join(tmp, "custom-path")

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", customDir,
		"--no-color",
	})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

func TestConfigFile_ExplicitFlag(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "my-config.yaml")

	err := os.MkdirAll(filepath.Dir(configPath), 0o755)
	require.NoError(t, err)

	err = os.WriteFile(configPath, []byte("vars: {}"), 0o644)
	require.NoError(t, err)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config", configPath,
		"--no-color",
	})

	err = rootCmd.Execute()
	assert.NoError(t, err)
}

func TestConfigFile_DerivesConfigDir(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "subdir", "config.yaml")

	err := os.MkdirAll(filepath.Dir(configPath), 0o755)
	require.NoError(t, err)

	err = os.WriteFile(configPath, []byte("vars: {}"), 0o644)
	require.NoError(t, err)

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config", configPath,
		"--no-color",
	})

	err = rootCmd.Execute()
	assert.NoError(t, err)
}