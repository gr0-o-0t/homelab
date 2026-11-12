package cmd

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAgent replaces forwardRun for one test and records what was forwarded.
// The leader it stands in for has no local images (up/update ask first).
func fakeAgent(t *testing.T, code int, err error) *[][]string {
	t.Helper()
	var calls [][]string
	prev, prevShip, prevAdd := forwardRun, shipLeader, addLeader
	forwardRun = func(argv []string) (int, error) {
		calls = append(calls, append([]string(nil), argv...))
		return code, err
	}
	shipLeader = func(_ context.Context, _ []string, _ io.Reader, stdout io.Writer) (int, error) {
		_, werr := io.WriteString(stdout, "[]\n")
		return 0, werr
	}
	// add <dir> sends its folder this way: recorded like a forwarded argv.
	addLeader = func(_ context.Context, argv []string, _ io.Reader, _ io.Writer) (int, error) {
		calls = append(calls, append([]string(nil), argv...))
		return code, err
	}
	t.Cleanup(func() {
		forwardRun, shipLeader, addLeader = prev, prevShip, prevAdd
		invokedArgs = nil
		resetFlags()
	})
	return &calls
}

func TestRoot_ForwardsNonLocalCommands(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	calls := fakeAgent(t, 7, nil)

	for _, argv := range [][]string{
		{"up", "web", "-a"},
		{"--no-color", "status", "web"},
		{"cf", "route", "add", "web"},
		{"exec", "web", "ls", "-la"},
	} {
		resetFlags()
		assert.Equal(t, 7, executeArgs(argv), "%q: the remote exit code is the process's", argv)
	}
	assert.Equal(t, [][]string{
		{"up", "web", "-a"},
		{"--no-color", "status", "web"},
		{"cf", "route", "add", "web"},
		{"exec", "web", "ls", "-la"},
	}, *calls)

	// A failure to reach the agent is a non-zero exit, not a local run.
	*calls = nil
	forwardRun = func([]string) (int, error) { return 1, errors.New("no agent") }
	resetFlags()
	assert.Equal(t, 1, executeArgs([]string{"up", "web"}))
}

func TestRoot_RunsLocalOnlyCommandsLocally(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	calls := fakeAgent(t, 7, nil)

	for _, argv := range [][]string{
		{"version"},
		{"completion", "bash"},
		{"help", "up"},
		{"up", "--help"},
		{"__complete", "u"},
	} {
		resetFlags()
		assert.Equal(t, 0, executeArgs(argv), "%q", argv)
	}
	assert.Empty(t, *calls)
}

func TestRoot_NeverForwardsInsideAgent(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "1")
	calls := fakeAgent(t, 7, nil)

	resetFlags()
	code := executeArgs([]string{"--config-dir", t.TempDir(), "ls", "-q"})
	require.Equal(t, 0, code)
	assert.Empty(t, *calls)
}
