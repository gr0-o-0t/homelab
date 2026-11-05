package cmd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/cmd"
)

// tor, i2p and ygg are not registered in cluster mode: asking for them on
// enable or disable (directly or through --all-style flags) fails with one
// fixed message instead of an unknown-flag error.
func TestEnable_TorRefusedInClusterMode(t *testing.T) {
	for _, layer := range []string{"tor", "i2p", "ygg"} {
		for _, verb := range []string{"enable", "disable"} {
			rootCmd := cmd.RootCmd()
			rootCmd.SetArgs([]string{"--config-dir", t.TempDir(), verb, "gitea", "--" + layer})
			err := rootCmd.Execute()
			require.Error(t, err, "%s --%s", verb, layer)
			assert.EqualError(t, err, layer+" is not available in cluster mode yet")
			// flags are package state: don't leak into later tests
			c, _, ferr := rootCmd.Find([]string{verb})
			require.NoError(t, ferr)
			require.NoError(t, c.Flags().Set(layer, "false"))
		}
	}
}
