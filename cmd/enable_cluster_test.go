package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/cmd"
	"github.com/groot/homelab/internal/exposure"
)

// resetFlag clears a package-global flag after the test, pass or fail.
func resetFlag(t *testing.T, verb, flag string) {
	t.Helper()
	t.Cleanup(func() {
		c, _, err := cmd.RootCmd().Find([]string{verb})
		if err == nil {
			_ = c.Flags().Set(flag, "false")
		}
	})
}

// tor, i2p and ygg are not registered in cluster mode: asking for them on
// enable fails with one fixed message instead of an unknown-flag error.
func TestEnable_TorRefusedInClusterMode(t *testing.T) {
	for _, layer := range []string{"tor", "i2p", "ygg"} {
		t.Run(layer, func(t *testing.T) {
			resetFlag(t, "enable", layer)
			rootCmd := cmd.RootCmd()
			rootCmd.SetArgs([]string{"--config-dir", t.TempDir(), "enable", "gitea", "--" + layer})
			err := rootCmd.Execute()
			require.Error(t, err, "enable --%s", layer)
			assert.EqualError(t, err, layer+" is not available in cluster mode yet")
		})
	}
}

func stateOf(t *testing.T, root, svc string) exposure.State {
	t.Helper()
	st, err := exposure.Load(root, svc)
	require.NoError(t, err)
	return st
}

func strandedService(t *testing.T, layers ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "services", "gitea")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("ports:\n  - 3000\n"), 0o600))
	require.NoError(t, exposure.Save(root, "gitea", exposure.State{Layers: layers}))
	return root
}

// An exposure.yaml written before cluster mode must not be stranded:
// `disable --tor` removes tor from it and leaves the other layers.
func TestDisable_RemovesUnservedLayerFromState(t *testing.T) {
	resetFlag(t, "disable", "tor")
	root := strandedService(t, "ts", "tor")
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", root, "disable", "gitea", "--tor"})
	require.NoError(t, rootCmd.Execute())
	assert.Equal(t, []string{"ts"}, stateOf(t, root, "gitea").Layers)
}

// -a removes every layer, served or not.
func TestDisable_AllRemovesUnservedLayers(t *testing.T) {
	t.Cleanup(func() {
		c, _, _ := cmd.RootCmd().Find([]string{"disable"})
		_ = c.Flags().Set("all", "false")
	})
	root := strandedService(t, "ts", "cf", "tor", "i2p", "ygg")
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", root, "disable", "gitea", "-a"})
	require.NoError(t, rootCmd.Execute())
	assert.Empty(t, stateOf(t, root, "gitea").Layers)
}

// A tor-only service: disabling tor leaves it on no layer, without error.
func TestDisable_TorOnlyService(t *testing.T) {
	resetFlag(t, "disable", "tor")
	root := strandedService(t, "tor")
	rootCmd := cmd.RootCmd()
	rootCmd.SetArgs([]string{"--config-dir", root, "disable", "gitea", "--tor"})
	require.NoError(t, rootCmd.Execute())
	assert.Empty(t, stateOf(t, root, "gitea").Layers)
}
