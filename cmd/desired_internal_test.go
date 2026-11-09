package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/desired"
)

// runCLI executes argv against a config dir. The docker calls behind it fail
// (TestMain points DOCKER_HOST nowhere); only what was recorded matters here.
func runCLI(t *testing.T, root string, argv ...string) {
	t.Helper()
	resetFlags()
	t.Cleanup(resetFlags)
	rootCmd.SetArgs(append([]string{"--config-dir", root}, argv...))
	_ = rootCmd.Execute()
}

func desiredFixture(t *testing.T) string {
	t.Helper()
	root := makeBatchFixture(t) // services alpha, beta
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("vars:\n  DOMAIN:\n    value: example.com\ngroups:\n  media: [alpha, beta]\n"), 0o644))
	return root
}

func loadDesired(t *testing.T, root string) *desired.Desired {
	t.Helper()
	d, exists, err := desired.LoadDesired(root)
	require.NoError(t, err)
	require.True(t, exists, "desired.yaml written")
	return d
}

func TestDesiredState_LifecycleTransitions(t *testing.T) {
	root := desiredFixture(t)
	steps := []struct {
		argv        []string
		alpha, beta desired.State
	}{
		{[]string{"start", "alpha"}, desired.Running, ""},
		{[]string{"up", "-a"}, desired.Running, desired.Running},
		{[]string{"stop", "--group", "media"}, desired.Stopped, desired.Stopped},
		{[]string{"restart", "beta"}, desired.Stopped, desired.Running},
		{[]string{"down", "beta"}, desired.Stopped, desired.Stopped},
		{[]string{"update", "alpha", "beta"}, desired.Running, desired.Running},
		{[]string{"down", "-a"}, desired.Stopped, desired.Stopped},
		{[]string{"prune", "--keep-volumes", "--yes", "alpha"}, "", desired.Stopped},
	}
	for _, s := range steps {
		runCLI(t, root, s.argv...)
		d := loadDesired(t, root)
		name := strings.Join(s.argv, " ")
		assert.Equal(t, s.alpha, d.Services["alpha"], "%s: alpha", name)
		assert.Equal(t, s.beta, d.Services["beta"], "%s: beta", name)
	}
}

func TestDesiredState_DeleteForgetsService(t *testing.T) {
	root := desiredFixture(t)
	runCLI(t, root, "up", "alpha", "beta")
	runCLI(t, root, "delete", "--yes", "--force", "alpha")
	d := loadDesired(t, root)
	_, ok := d.Services["alpha"]
	assert.False(t, ok, "deleted services leave the desired state")
	assert.Equal(t, desired.Running, d.Services["beta"])
}

func TestDesiredState_UnknownServiceNotRecorded(t *testing.T) {
	root := desiredFixture(t)
	runCLI(t, root, "up", "nosuch")
	_, exists, err := desired.LoadDesired(root)
	require.NoError(t, err)
	assert.False(t, exists)
}

// With no service named, the lifecycle verbs refuse: there is no core stack
// to act on, and nothing is recorded.
func TestDesiredState_NoServiceRecordsNothing(t *testing.T) {
	root := desiredFixture(t)
	for _, verb := range []string{"up", "down", "start", "stop", "restart", "pull", "update"} {
		runCLI(t, root, verb)
	}
	_, exists, err := desired.LoadDesired(root)
	require.NoError(t, err)
	assert.False(t, exists)
}
