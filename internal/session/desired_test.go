package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDesired_MissingFile(t *testing.T) {
	d, exists, err := LoadDesired(t.TempDir())
	require.NoError(t, err)
	assert.False(t, exists)
	assert.NotNil(t, d.Services)
	assert.False(t, d.CoreRunning())
}

func TestDesired_RoundTripAndTransitions(t *testing.T) {
	root := t.TempDir()
	d := &Desired{}
	d.SetCore(Running)
	d.SetService("jellyfin", Running)
	d.SetService("immich", Running)
	d.SetService("immich", Stopped)
	d.SetService("gone", Running)
	d.SetService("gone", Removed)
	require.NoError(t, d.Save(root))

	got, exists, err := LoadDesired(root)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.True(t, got.CoreRunning())
	assert.True(t, got.ServiceRunning("jellyfin"))
	assert.False(t, got.ServiceRunning("immich"))
	assert.Equal(t, Stopped, got.Services["immich"])
	_, present := got.Services["gone"]
	assert.False(t, present, "removed services are forgotten")

	got.SetCore(Removed) // the core cannot be removed, only stopped
	assert.Equal(t, Stopped, got.Core)

	info, err := os.Stat(StateDir(root))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestDesired_RunningServices_SharedFirst(t *testing.T) {
	d := &Desired{Services: map[string]State{
		"zeta": Running, "alpha": Running, "redis": Running, "postgres": Running, "off": Stopped,
	}}
	assert.Equal(t, []string{"postgres", "redis", "alpha", "zeta"}, d.RunningServices())
}

func TestDesired_CorruptFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(StateDir(root), 0o700))
	require.NoError(t, os.WriteFile(DesiredFile(root), []byte("core: [nope"), 0o600))
	_, _, err := LoadDesired(root)
	assert.Error(t, err)
}
