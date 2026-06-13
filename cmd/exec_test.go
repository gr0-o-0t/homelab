package cmd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/cmd"
)

func TestExecCommand_MissingService(t *testing.T) {
	tmp := t.TempDir()
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "exec", "nonexistent", "sh"})
	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestExecCommand_NoArgs(t *testing.T) {
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"exec"})
	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestExecCommand_FlagsRegistered(t *testing.T) {
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"exec", "--help"})
	err := rootCmd.Execute()
	assert.NoError(t, err)
}
