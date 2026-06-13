package cmd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/cmd"
)

func TestPortCommand_MissingService(t *testing.T) {
	tmp := t.TempDir()
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "port", "nonexistent", "8080"})
	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestPortCommand_NoArgs(t *testing.T) {
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"port"})
	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestPortCommand_OneArg(t *testing.T) {
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"port", "jellyfin"})
	err := rootCmd.Execute()
	assert.Error(t, err)
}
