package remote

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunner_AliasesTakeTheLock(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")

	var wg sync.WaitGroup
	for _, argv := range [][]string{{"up", "a", "400"}, {"rm", "b", "400"}} {
		wg.Go(func() {
			code, _, _, err := run(t, c, RunOptions{Argv: argv})
			assert.NoError(t, err)
			assert.Equal(t, 0, code)
		})
	}
	wg.Wait()
	as, ae := span(t, e.stateDir, "a")
	bs, be := span(t, e.stateDir, "b")
	assert.True(t, ae <= bs || be <= as, "`rm` (delete) ran unlocked beside `up`: a=[%d,%d] b=[%d,%d]", as, ae, bs, be)
}

func TestRemote_RefusesUnknownInternalAndNoRecord(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")
	for _, argv := range [][]string{
		{"frobnicate"},
		{"session", "run"},
		{"agent"},
		{"envshim", "--", "/bin/sh"},
		{"ls", "--no-record"},
		{"--no-record", "ls"},
		{"ls", "--no-record=true"},
	} {
		code, out, _, err := run(t, c, RunOptions{Argv: argv})
		require.Error(t, err, "%q", argv)
		assert.Equal(t, 2, code, "%q", argv)
		assert.NotContains(t, out, "ARGV=", "%q ran", argv)
	}
}

func TestRemote_NoResolverRefusesEverything(t *testing.T) {
	e := newTestServer(t, func(c *ServerConfig) { c.Resolve = nil })
	code, _, _, err := run(t, e.client(testSecret, "1.4.2"), RunOptions{Argv: []string{"status"}})
	require.Error(t, err)
	assert.Equal(t, 2, code)
}

func waitPid(t *testing.T, dir, id string) int {
	t.Helper()
	f := filepath.Join(dir, id+".pid")
	require.Eventually(t, func() bool { _, err := os.Stat(f); return err == nil }, 10*time.Second, 20*time.Millisecond)
	b, _ := os.ReadFile(f)
	pid, _ := strconv.Atoi(string(b))
	require.NotZero(t, pid)
	return pid
}

// A client that stops reading — and so stops answering pings — without
// closing its connection is dropped after two ping intervals: its child is
// killed and the lock released.
func TestRemote_SilentClientIsDropped(t *testing.T) {
	e := newTestServer(t, func(c *ServerConfig) { c.PingInterval = 100 * time.Millisecond })
	tr := e.srv.Client().Transport.(*http.Transport)
	d := websocket.Dialer{TLSClientConfig: tr.TLSClientConfig.Clone()}
	url := "wss" + strings.TrimPrefix(e.srv.URL, "https") + Path
	conn, _, err := d.Dial(url, http.Header{"Authorization": {"Bearer " + Token(testSecret)}})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.WriteJSON(Handshake{Argv: []string{"up", "silent", "600000"}, Version: "1.4.2"}))
	// …and never reads again.

	pid := waitPid(t, e.stateDir, "silent")
	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 10*time.Second, 20*time.Millisecond,
		"child of a silent client survived")

	start := time.Now()
	code, _, _, err := run(t, e.client(testSecret, "1.4.2"), RunOptions{Argv: []string{"up", "next", "10"}})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestRemote_CapsConcurrentSessions(t *testing.T) {
	e := newTestServer(t, func(c *ServerConfig) { c.MaxSessions = 1 })
	c := e.client(testSecret, "1.4.2")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Run(ctx, RunOptions{Argv: []string{"up", "hold", "600000"}, Stdin: strings.NewReader(""),
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	}()
	waitPid(t, e.stateDir, "hold")

	code, _, _, err := run(t, c, RunOptions{Argv: []string{"status"}})
	require.Error(t, err)
	assert.Equal(t, 2, code)
	assert.Contains(t, err.Error(), "busy")

	cancel()
	<-done
	// The slot is freed with the session.
	require.Eventually(t, func() bool {
		code, _, _, err := run(t, c, RunOptions{Argv: []string{"status"}})
		return err == nil && code == 3
	}, 15*time.Second, 100*time.Millisecond)
}
