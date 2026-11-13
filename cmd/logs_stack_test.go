package cmd

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTimedLogs answers `docker service logs` with fixed timestamped output
// per Swarm service: backlog for a call without --follow, follow for one
// with it. It records every argv.
func stubTimedLogs(t *testing.T, backlog, follow map[string]string) *[][]string {
	t.Helper()
	return stubLogsFn(t, func(name string, following bool) (string, string, error) {
		if following {
			return follow[name], "", nil
		}
		return backlog[name], "", nil
	})
}

// stubLogsFn answers `docker service logs` with fn's stdout, stderr and
// error for the Swarm service (and whether it follows), recording every argv.
func stubLogsFn(t *testing.T, fn func(name string, follow bool) (string, string, error)) *[][]string {
	t.Helper()
	var mu sync.Mutex
	var got [][]string
	old := serviceLogsCLI
	serviceLogsCLI = func(_ context.Context, args []string, stdout, stderr io.Writer) error {
		mu.Lock()
		got = append(got, args)
		mu.Unlock()
		out, errOut, err := fn(args[len(args)-1], slices.Contains(args, "--follow"))
		_, _ = io.WriteString(stdout, out)
		_, _ = io.WriteString(stderr, errOut)
		return err
	}
	t.Cleanup(func() { serviceLogsCLI = old })
	return &got
}

// What `docker service logs --tail 5 --timestamps` gives for a service with
// an old task (Nov 12) and the current one (Nov 13): each task's last lines,
// one task after the other, not in time order.
const (
	serverBacklog = "2026-11-13T09:00:01.000000000Z new 1\n" +
		"2026-11-13T09:00:03.000000000Z new 2\n" +
		"2026-11-13T09:00:05.000000000Z new 3\n" +
		"2026-11-12T20:00:01.000000000Z old 1\n" +
		"2026-11-12T20:00:02.000000000Z old 2\n" +
		"2026-11-12T20:00:03.000000000Z old 3\n"
	mlBacklog = "2026-11-13T09:00:02.000000000Z ml 1\n" +
		"2026-11-13T09:00:04.000000000Z ml 2\n" +
		"    at continuation of ml 2\n" +
		"2026-11-12T20:00:04.000000000Z ml old\n"
)

func immichTimedLogs(t *testing.T, follow map[string]string) (string, *[][]string) {
	t.Helper()
	root := makeBatchFixture(t)
	immichStack(t, root, fakeSwarm(t))
	got := stubTimedLogs(t, map[string]string{
		"immich_immich-server":           serverBacklog,
		"immich_immich-machine-learning": mlBacklog,
	}, follow)
	return root, got
}

// -n 5 gives the last 5 entries of the whole stack, every task and
// container merged by time, oldest first; the timestamps docker added are
// stripped. An entry is a stamped line plus the unstamped lines after it:
// real docker output stamps every line, so there an entry is one line; here
// "ml 2" carries a continuation, so 5 entries print 6 lines.
func TestLogs_TailMergedByTime(t *testing.T) {
	root, got := immichTimedLogs(t, nil)
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-n", "5", "immich")
	require.Zero(t, code)
	assert.Equal(t, ""+
		"immich-server           | new 1\n"+
		"immich-machine-learning | ml 1\n"+
		"immich-server           | new 2\n"+
		"immich-machine-learning | ml 2\n"+
		"immich-machine-learning |     at continuation of ml 2\n"+
		"immich-server           | new 3\n", out)
	assert.ElementsMatch(t, [][]string{
		svcLogsArgv("--tail", "5", "immich_immich-machine-learning"),
		svcLogsArgv("--tail", "5", "immich_immich-server"),
	}, *got)
}

// -n 5 on one container: exactly 5 lines across its old and new task.
func TestLogs_TailAcrossTasks(t *testing.T) {
	root, _ := immichTimedLogs(t, nil)
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-n", "5", "-c", "immich-server", "immich")
	require.Zero(t, code)
	assert.Equal(t, []string{"old 2", "old 3", "new 1", "new 2", "new 3"}, stripPrefixes(out))
}

// -t keeps docker's timestamps.
func TestLogs_TimestampsKept(t *testing.T) {
	root, _ := immichTimedLogs(t, nil)
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-t", "-n", "2", "-c", "immich-server", "immich")
	require.Zero(t, code)
	assert.Equal(t, []string{"2026-11-13T09:00:03.000000000Z new 2", "2026-11-13T09:00:05.000000000Z new 3"}, stripPrefixes(out))
}

// Without -n nothing is buffered or merged: every line is streamed as
// docker prints it (task after task), timestamps stripped; with -f it is
// the one following call, as before.
func TestLogs_NoTailStreamsUnmerged(t *testing.T) {
	root, got := immichTimedLogs(t, map[string]string{"immich_immich-server": serverBacklog})
	for _, argv := range [][]string{{"logs", "-c", "immich-server", "immich"}, {"logs", "-n", "all", "-c", "immich-server", "immich"}} {
		*got = nil
		code, out, _ := runLogs(t, append([]string{"--config-dir", root}, argv...)...)
		require.Zero(t, code)
		assert.Equal(t, []string{"new 1", "new 2", "new 3", "old 1", "old 2", "old 3"}, stripPrefixes(out), "%v", argv)
		require.Len(t, *got, 1)
	}
	assert.Equal(t, svcLogsArgv("--tail", "all", "immich_immich-server"), (*got)[0])

	*got = nil
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-f", "-c", "immich-server", "immich")
	require.Zero(t, code)
	assert.Equal(t, []string{"new 1", "new 2", "new 3", "old 1", "old 2", "old 3"}, stripPrefixes(out))
	assert.Equal(t, [][]string{svcLogsArgv("--follow", "immich_immich-server")}, *got)
}

// Container stderr lines are merged with stdout by time and counted by -n,
// each printed to its own stream; docker's own unstamped error text comes
// after the log lines and is never cut.
func TestLogs_StderrMerged(t *testing.T) {
	root := makeBatchFixture(t)
	immichStack(t, root, fakeSwarm(t))
	stubLogsFn(t, func(name string, _ bool) (string, string, error) {
		if name == "immich_immich-server" {
			return serverBacklog, "2026-11-13T09:00:04.000000000Z warn between\n", nil
		}
		return "", "Error response from daemon: rpc error\n", errors.New("exit status 1")
	})
	code, out, errOut := runLogs(t, "--config-dir", root, "logs", "-n", "3", "immich")
	assert.NotZero(t, code)
	assert.Equal(t, []string{"new 2", "new 3"}, stripPrefixes(out))
	assert.True(t, strings.HasPrefix(errOut, ""+
		"immich-server           | warn between\n"+
		"immich-machine-learning | Error response from daemon: rpc error\n"), "%q", errOut)
}

// Several identical lines at the cutoff: as many as the backlog had are
// skipped when following, the rest are new.
func TestLogs_FollowRepeatedLinesAtCutoff(t *testing.T) {
	const tick = "2026-11-13T09:00:05.000000000Z tick\n"
	root := makeBatchFixture(t)
	immichStack(t, root, fakeSwarm(t))
	stubLogsFn(t, func(_ string, follow bool) (string, string, error) {
		if follow {
			return tick + tick + tick + "2026-11-13T09:00:06.000000000Z after\n", "", nil
		}
		return "2026-11-13T09:00:04.000000000Z before\n" + tick + tick, "", nil
	})
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-f", "-n", "5", "-c", "immich-server", "immich")
	require.Zero(t, code)
	assert.Equal(t, []string{"before", "tick", "tick", "tick", "after"}, stripPrefixes(out))
}

// A service whose backlog fails is not followed; the others are, and the
// error is reported at the end.
func TestLogs_FollowPastFailedBacklog(t *testing.T) {
	root := makeBatchFixture(t)
	immichStack(t, root, fakeSwarm(t))
	got := stubLogsFn(t, func(name string, follow bool) (string, string, error) {
		switch {
		case name == "immich_immich-machine-learning":
			return "", "incomplete log stream: node is not available\n", errors.New("exit status 1")
		case follow:
			return "2026-11-13T09:00:06.000000000Z live\n", "", nil
		}
		return serverBacklog, "", nil
	})
	code, out, errOut := runLogs(t, "--config-dir", root, "logs", "-f", "-n", "1", "immich")
	assert.NotZero(t, code)
	assert.Equal(t, []string{"new 3", "live"}, stripPrefixes(out))
	assert.Contains(t, errOut, "machine-learning | incomplete log stream") // the backlog error is reported
	assert.Len(t, *got, 4)                                                 // two backlogs, two follows
	var mlFollow bool
	for _, a := range *got {
		mlFollow = mlFollow || (slices.Contains(a, "--follow") && a[len(a)-1] == "immich_immich-machine-learning")
	}
	assert.True(t, mlFollow, "the service whose backlog failed is followed too")
}

// -f prints the merged, tailed backlog, then follows each service from the
// last line its backlog had: lines already printed are not repeated.
func TestLogs_FollowAfterBacklog(t *testing.T) {
	root, got := immichTimedLogs(t, map[string]string{
		"immich_immich-server": "2026-11-13T09:00:05.000000000Z new 3\n" + // already printed
			"2026-11-13T09:00:06.000000000Z new 4\n",
		"immich_immich-machine-learning": "",
	})
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-f", "-n", "2", "immich")
	require.Zero(t, code)
	assert.Equal(t, ""+
		"immich-machine-learning | ml 2\n"+
		"immich-machine-learning |     at continuation of ml 2\n"+
		"immich-server           | new 3\n"+
		"immich-server           | new 4\n", out)
	assert.Contains(t, *got, svcLogsArgv("--follow",
		"--since", "2026-11-13T09:00:05Z", "immich_immich-server"))
	assert.Contains(t, *got, svcLogsArgv("--follow",
		"--since", "2026-11-13T09:00:04Z", "immich_immich-machine-learning"))
}

// A service with an empty backlog follows from now (--tail 0), or from the
// user's --since.
func TestLogs_FollowEmptyBacklog(t *testing.T) {
	root := makeBatchFixture(t)
	immichStack(t, root, fakeSwarm(t))
	got := stubTimedLogs(t, nil, map[string]string{"immich_immich-server": "2026-11-13T09:00:06.000000000Z live\n"})
	before := time.Now().Add(-time.Second)
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-f", "-n", "5", "-c", "immich-server", "immich")
	after := time.Now().Add(time.Second)
	require.Zero(t, code)
	assert.Equal(t, "immich-server | live\n", out)
	// From when the backlog read began, so nothing logged in between is lost.
	f := (*got)[1]
	require.Len(t, f, 8, f)
	assert.Equal(t, svcLogsArgv("--follow", "--since")[:6], f[:6])
	assert.Equal(t, "immich_immich-server", f[7])
	since, err := time.Parse(time.RFC3339Nano, f[6])
	require.NoError(t, err)
	assert.True(t, since.After(before) && since.Before(after), since)

	*got = nil
	code, _, _ = runLogs(t, "--config-dir", root, "logs", "-f", "-n", "5", "--since", "1h", "-c", "immich-server", "immich")
	require.Zero(t, code)
	assert.Equal(t, svcLogsArgv("--tail", "5", "--since", "1h", "immich_immich-server"), (*got)[0])
	assert.Equal(t, svcLogsArgv("--follow", "--since", "1h", "immich_immich-server"), (*got)[1])
}

func TestLogs_BadTailRefused(t *testing.T) {
	root, got := immichTimedLogs(t, nil)
	code, _, _ := runLogs(t, "--config-dir", root, "logs", "-n", "five", "immich")
	assert.NotZero(t, code)
	assert.Empty(t, *got)
}

// stripPrefixes drops the "key | " prefix of every output line.
func stripPrefixes(out string) []string {
	var lines []string
	for l := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		_, rest, _ := strings.Cut(l, " | ")
		lines = append(lines, rest)
	}
	return lines
}

// svcLogsArgv is the `docker service logs` argv runStackLogs builds: args
// between the fixed flags and the Swarm service name (args' last).
func svcLogsArgv(args ...string) []string {
	return append([]string{"service", cmdLogs, "--raw", "--timestamps"}, args...)
}
