package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDisableFlags_ShorthandAMapsToAll guards the documented behavior
// ("homelab disable <svc> -a  # remove all layers + stop container"):
// -a must be the shorthand for --all, not --stop, and --all must resolve to
// removing every extension layer plus stopping the container.
func TestDisableFlags_ShorthandAMapsToAll(t *testing.T) {
	flag := disableCmd.Flags().ShorthandLookup("a")
	require.NotNil(t, flag, "expected -a to be a registered shorthand")
	assert.Equal(t, "all", flag.Name, "-a must be the shorthand for --all")

	stopFlag := disableCmd.Flags().Lookup("stop")
	require.NotNil(t, stopFlag)
	assert.Empty(t, stopFlag.Shorthand, "--stop should have no shorthand of its own")
}

// TestDisableAll_ImpliesStop calls the real runDisable (against a
// nonexistent service in a temp config dir, which every layer handles
// gracefully — see TestDisableCommand_MissingService) with only -a set, and
// checks that it flipped disableCf/i2p/tor/ygg/stop itself. This exercises
// runDisable's actual resolution block rather than a re-implementation of it.
func TestDisableAll_ImpliesStop(t *testing.T) {
	origCf, origI2P, origTor, origYgg, origAll, origStop :=
		disableCf, disableI2P, disableTor, disableYgg, disableAll, disableStop
	t.Cleanup(func() {
		disableCf, disableI2P, disableTor, disableYgg, disableAll, disableStop =
			origCf, origI2P, origTor, origYgg, origAll, origStop
	})

	disableCf, disableI2P, disableTor, disableYgg, disableStop = false, false, false, false, false
	disableAll = true

	origConfigDir := rootFlags.configDir
	rootFlags.configDir = t.TempDir()
	t.Cleanup(func() { rootFlags.configDir = origConfigDir })

	require.NoError(t, runDisable(disableCmd, []string{"nonexistent"}))

	assert.True(t, disableCf)
	assert.True(t, disableI2P)
	assert.True(t, disableTor)
	assert.True(t, disableYgg)
	assert.True(t, disableStop)
}
