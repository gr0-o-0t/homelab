package cmd_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/cmd"
)

func TestStatusTable_RendersWithoutError(t *testing.T) {
	tmp := t.TempDir()
	svcDir := filepath.Join(tmp, "services", "testapp")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcDir, "docker-compose.yml"),
		[]byte("services: {}\n"), 0o644,
	))

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "status"})
	err := rootCmd.Execute()
	assert.NoError(t, err, "status command must produce no error with a service dir")
}

func TestStatusTable_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "status"})
	err := rootCmd.Execute()
	assert.NoError(t, err, "status command must not error on empty config dir")
}

func TestStatusCoreTable_RendersCoreSection(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "services"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "caddy", "conf.d"), 0o755))

	// Create a service so the full command path is exercised
	svcDir := filepath.Join(tmp, "services", "testapp")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcDir, "docker-compose.yml"),
		[]byte("services: {}\n"), 0o644,
	))

	// Redirect stdout to capture table output
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", tmp, "status"})
	err = rootCmd.Execute()
	require.NoError(t, err)

	require.NoError(t, w.Close())
	os.Stdout = old

	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "SERVICE", "table header must contain SERVICE column label")
	assert.Contains(t, output, "STATE", "table header must contain STATE column label")
	assert.Contains(t, output, "caddy", "caddy must appear in core table")

	// TestApp should appear in the services section
	assert.Contains(t, output, "testapp", "test service must appear in output")
}
