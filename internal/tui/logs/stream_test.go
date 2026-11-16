package logs

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func collect(t *testing.T, ch <-chan string) []string {
	t.Helper()
	var lines []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-ch:
			if !ok {
				return lines
			}
			lines = append(lines, l)
		case <-timeout:
			t.Fatalf("stream did not end; got %d lines", len(lines))
		}
	}
}

// A log line longer than bufio.Scanner's 64 KiB default must not end the
// stream: the lines after it still arrive.
func TestLogStream_LongLine(t *testing.T) {
	ch, stop := startLogStream([]string{"sh", "-c", `head -c 100000 /dev/zero | tr '\0' a; echo; echo after`})
	defer stop()
	lines := collect(t, ch)
	require.NotEmpty(t, lines)
	assert.Equal(t, "after", lines[len(lines)-1])
}

// Stopping the viewer kills the whole process group: `homelab logs` runs
// `docker service logs -f` below it, which must not outlive the viewer.
func TestLogStream_StopKillsGrandchildren(t *testing.T) {
	ch, stop := startLogStream([]string{"sh", "-c", `sleep 300 & echo $!; wait`})
	var first string
	select {
	case first = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no pid line")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(first))
	require.NoError(t, err)
	stop()
	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 10*time.Second, 20*time.Millisecond,
		"the grandchild (sleep) outlived the viewer")
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
