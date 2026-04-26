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
		"caddy", "validate",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestServiceUp_MissingService(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"service", "up", "nonexistent-service",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestServiceDown_MissingService(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"service", "down", "nonexistent-service",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestServiceEnable_MissingService(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"service", "enable", "nonexistent-service",
	})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestServiceDisable_MissingService(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{
		"--config-dir", tmp,
		"service", "disable", "nonexistent-service",
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
	rootCmd.SetArgs([]string{"--config-dir", tmp, "service", "update"})

	err := rootCmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

func TestServiceUpdate_MissingService(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "service", "update", "nonexistent"})

	err := rootCmd.Execute()
	assert.Error(t, err)
}

func TestServiceUpdate_AllEmpty(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "services"), 0o755))

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "service", "update", "--all"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

// ── service enable / disable ──────────────────────────────────────────────────

func TestServiceEnable_MissingRouteFlags(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "service", "enable", "jellyfin"})

	err := rootCmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--private")
}

// ── service up --all ──────────────────────────────────────────────────────────

func TestServiceUp_AllEmpty(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "services"), 0o755))

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "service", "up", "--all"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

// ── service doctor ────────────────────────────────────────────────────────────

func TestServiceDoctor_NoArgsNoAll(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "service", "doctor"})

	err := rootCmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

func TestServiceDoctor_AllNoServices(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "services"), 0o755))

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "service", "doctor", "--all"})

	err := rootCmd.Execute()
	assert.NoError(t, err)
}

// ── tunnel ────────────────────────────────────────────────────────────────────

func TestTunnelStatus_NotConfigured(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "tunnel", "status"})

	err := rootCmd.Execute()
	assert.NoError(t, err) // prints "Not configured" but does not error
}

func TestTunnelRouteAdd_MissingConfig(t *testing.T) {
	tmp := t.TempDir()

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "tunnel", "route", "add", "jellyfin"})

	err := rootCmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CF_TUNNEL")
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