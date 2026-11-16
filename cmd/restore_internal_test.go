package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/backup"
)

// A backup of a service that is not installed is refused before the
// "replaces current data" prompt, not after the operator said yes.
func TestRestore_UninstalledServiceRefusedBeforePrompt(t *testing.T) {
	prevRoot, prevDir := backupsRoot, rootFlags.configDir
	t.Cleanup(func() { backupsRoot, rootFlags.configDir = prevRoot, prevDir })
	backupsRoot, rootFlags.configDir = t.TempDir(), t.TempDir()
	resetFlags()

	b := filepath.Join(backupsRoot, "b1")
	require.NoError(t, os.MkdirAll(b, 0o750))
	manifest := `{"version": ` + strconv.Itoa(backup.ManifestVersion) + `, "created": "2026-11-01T00:00:00Z", "services": [{"name": "web"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(b, backup.ManifestName), []byte(manifest), 0o600))

	t.Cleanup(resetFlags)
	rootCmd.SetArgs([]string{"--config-dir", rootFlags.configDir, "restore", "b1"})
	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Install it first")
}
