package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/cmd"
)

func TestCoreStatus_MissingConfigDir(t *testing.T) {
	tmp := t.TempDir()
	nonexistent := filepath.Join(tmp, "nonexistent")

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", nonexistent,
		"core", "status",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestCoreStart_MissingCoreStack(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"core", "start",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestCoreStop_MissingCoreStack(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"core", "stop",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestTailscaleStatus_MissingConfigDir(t *testing.T) {
	tmp := t.TempDir()
	nonexistent := filepath.Join(tmp, "nonexistent")

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", nonexistent,
		"ts", "status",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestCaddyValidate_MissingCaddyfile(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"validate",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestServiceUp_MissingService(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"up", "nonexistent-service",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestServiceDown_MissingService(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"down", "nonexistent-service",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestHelpCommand(t *testing.T) {
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--help"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

func TestVersionFlag(t *testing.T) {
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--version"})

	err := rootCmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown")
}

// ── service update ────────────────────────────────────────────────────────────

func TestServiceUpdate_NoArgs(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "update"})

	err := rootCmd.Execute()
	assert.Error(t, err) // core stack update fails because no Docker containers
}

func TestServiceUpdate_MissingService(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "update", "nonexistent"})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestServiceUpdate_AllEmpty(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "services"), 0o755))

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "update", "--all"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

// ── up --all ──────────────────────────────────────────────────────────────────

func TestServiceUp_AllEmpty(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "services"), 0o755))

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "up", "--all"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

// ── service doctor ────────────────────────────────────────────────────────────

func TestServiceDoctor_NoArgsNoAll(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "doctor"})

	err := rootCmd.Execute()
	assert.NoError(t, err) // root doctor runs health checks, succeeds
}

func TestServiceDoctor_AllNoServices(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "services"), 0o755))

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "doctor", "--all"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

// ── tunnel ────────────────────────────────────────────────────────────────────

func TestTunnelStatus_NotConfigured(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "ext", "cf", "status"})

	err := rootCmd.Execute()
	assert.NoError(t, err) // prints "Not configured" but does not error
}

func TestTunnelRouteAdd_NotEnabled(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "ext", "cf", "route", "add", "jellyfin"})

	err := rootCmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not enabled")
}

// ── completion ────────────────────────────────────────────────────────────────

func TestCompletionCommand_Bash(t *testing.T) {
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"completion", "bash"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

func TestCompletionCommand_InvalidShell(t *testing.T) {
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"completion", "nushell"})

	err := rootCmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}
