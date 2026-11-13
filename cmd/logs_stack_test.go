package cmd

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTimedLogs answers `docker service logs` with fixed timestamped output
// per Swarm service: backlog for a call without --follow, follow for one
// with it. It records every argv.
func stubTimedLogs(t *testing.T, backlog, follow map[string]string) *[][]string {
	t.Helper()
	var mu sync.Mutex
	var got [][]string
	old := serviceLogsCLI
	serviceLogsCLI = func(_ context.Context, args []string, stdout, _ io.Writer) error {
		mu.Lock()
		got = append(got, args)
		mu.Unlock()
		name := args[len(args)-1]
		if slices.Contains(args, "--follow") {
			_, _ = io.WriteString(stdout, follow[name])
		} else {
			_, _ = io.WriteString(stdout, backlog[name])
		}
		return nil
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

// -n 5 gives the last 5 lines of the whole stack, every task and container
// merged by time, oldest first; the timestamps docker added are stripped,
// and a line without one stays with the line before it.
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

// Without -n every line is printed, still in time order.
func TestLogs_AllLinesInTimeOrder(t *testing.T) {
	root, _ := immichTimedLogs(t, nil)
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-c", "immich-server", "immich")
	require.Zero(t, code)
	assert.Equal(t, []string{"old 1", "old 2", "old 3", "new 1", "new 2", "new 3"}, stripPrefixes(out))
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
	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-f", "-c", "immich-server", "immich")
	require.Zero(t, code)
	assert.Equal(t, "immich-server | live\n", out)
	assert.Equal(t, svcLogsArgv("--follow", "--tail", "0", "immich_immich-server"), (*got)[1])

	*got = nil
	code, _, _ = runLogs(t, "--config-dir", root, "logs", "-f", "--since", "1h", "-c", "immich-server", "immich")
	require.Zero(t, code)
	assert.Equal(t, svcLogsArgv("--since", "1h", "immich_immich-server"), (*got)[0])
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
