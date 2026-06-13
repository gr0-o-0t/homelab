package cmd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/cmd"
)

func TestImagesCommand_MissingService(t *testing.T) {
	tmp := t.TempDir()
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "images", "nonexistent"})
	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestImagesCommand_QuietFlag_MissingService(t *testing.T) {
	tmp := t.TempDir()
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "images", "--quiet", "nonexistent"})
	err := rootCmd.Execute()
	assert.Error(t, err)
}
