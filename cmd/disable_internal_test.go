package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDisableFlags_ShorthandAMapsToAll guards -a as the shorthand for --all
// (every layer), with --stop a separate, unabbreviated opt-in.
func TestDisableFlags_ShorthandAMapsToAll(t *testing.T) {
	flag := disableCmd.Flags().ShorthandLookup("a")
	require.NotNil(t, flag, "expected -a to be a registered shorthand")
	assert.Equal(t, "all", flag.Name, "-a must be the shorthand for --all")

	stopFlag := disableCmd.Flags().Lookup("stop")
	require.NotNil(t, stopFlag)
	assert.Empty(t, stopFlag.Shorthand, "--stop should have no shorthand of its own")
}

// TestDisableAll_DoesNotStop checks that -a only removes layers: against a
// service with no compose file, stopping would fail, so -a alone must succeed
// and -a --stop must not.
func TestDisableAll_DoesNotStop(t *testing.T) {
	origAll, origStop := disableAll, disableStop
	origConfigDir := rootFlags.configDir
	t.Cleanup(func() {
		disableAll, disableStop = origAll, origStop
		rootFlags.configDir = origConfigDir
	})
	rootFlags.configDir = t.TempDir()

	disableAll, disableStop = true, false
	require.NoError(t, runDisable(disableCmd, []string{"nonexistent"}))

	disableStop = true
	assert.Error(t, runDisable(disableCmd, []string{"nonexistent"}), "--stop must take the service down")
}

func TestDisable_RejectsPathNames(t *testing.T) {
	assert.Error(t, runDisable(disableCmd, []string{"../core"}))
}
