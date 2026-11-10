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

func TestRemote_RefusesUnknownAndInternal(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")
	for _, argv := range [][]string{
		{"frobnicate"},
		{"session", "run"},
		{"agent"},
		{"envshim", "--", "/bin/sh"},
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

func TestLocks_FollowTheCommandPath(t *testing.T) {
	for path, want := range map[string]bool{
		"up": true, "delete": true, "cf route add": true, "cf route rm": true,
		"ext enable": true, "ext disable": true, "ext start": true, "ext stop": true,
		"cf status": false, "cf logs": false, "ext list": false, "ext status": false, "ext logs": false,
		"status": false, "logs": false, "session run": false,
		"files put": true, "files get": false,
		"backup": true, "backup-list": false,
	} {
		assert.Equal(t, want, Locks(strings.Fields(path)), path)
	}
	// The decision is the resolved command path alone: no argv spelling of
	// backup escapes the lock, whatever cobra would make of it in the child.
	for argv, want := range map[string]bool{
		"backup --list": true, "backup --list --json": true, "backup web": true, "backup --all": true,
		"backup --all --out --list": true, "backup --group media --list": true, "backup --group --list": true,
		"backup --fetch --list": true, "backup --all -- --list": true, "backup --all --list --list=false": true,
		"backup-list --json": false, "backup-list": false,
	} {
		path, ok := testResolve(strings.Fields(argv))
		require.True(t, ok, argv)
		assert.Equal(t, want, Locks(path), argv)
	}
}

// A mutating backup whose argv merely contains --list (here as the value of
// --out) waits for the lock like any backup.
func TestRunner_BackupWithListValueTakesTheLock(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")
	var wg sync.WaitGroup
	wg.Go(func() {
		code, _, _, err := run(t, c, RunOptions{Argv: []string{"up", "a", "1000"}})
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
	})
	waitPid(t, e.stateDir, "a")
	code, _, _, err := run(t, c, RunOptions{Argv: []string{"backup", "--all", "--out", "--list", "b", "50"}})
	require.NoError(t, err)
	require.Equal(t, 0, code)
	wg.Wait()
	_, ae := span(t, e.stateDir, "a")
	bs, be := span(t, e.stateDir, "b")
	assert.True(t, ae <= bs, "backup --all --out --list ran unlocked: a ends %d, b=[%d,%d]", ae, bs, be)
}

// backup-list runs while an up holds the lock: the front ends list backups
// on every refresh.
func TestRunner_BackupListSkipsTheLock(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")
	var wg sync.WaitGroup
	wg.Go(func() {
		code, _, _, err := run(t, c, RunOptions{Argv: []string{"up", "a", "1500"}})
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
	})
	waitPid(t, e.stateDir, "a")
	code, _, _, err := run(t, c, RunOptions{Argv: []string{"backup-list", "--json", "b", "50"}})
	require.NoError(t, err)
	require.Equal(t, 0, code)
	wg.Wait()
	as, ae := span(t, e.stateDir, "a")
	bs, be := span(t, e.stateDir, "b")
	assert.True(t, as < bs && be < ae, "backup-list waited on the lock: a=[%d,%d] b=[%d,%d]", as, ae, bs, be)
}

func TestRunner_ReadOnlySubcommandsSkipTheLock(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")

	// cf logs runs inside a running up.
	var wg sync.WaitGroup
	wg.Go(func() {
		code, _, _, err := run(t, c, RunOptions{Argv: []string{"up", "a", "1500"}})
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
	})
	waitPid(t, e.stateDir, "a")
	code, _, _, err := run(t, c, RunOptions{Argv: []string{"cf", "logs", "b", "50"}})
	require.NoError(t, err)
	require.Equal(t, 0, code)
	wg.Wait()
	as, ae := span(t, e.stateDir, "a")
	bs, be := span(t, e.stateDir, "b")
	assert.True(t, as < bs && be < ae, "cf logs waited on the lock: a=[%d,%d] b=[%d,%d]", as, ae, bs, be)

	// cf route add takes it.
	for _, argv := range [][]string{{"up", "c", "400"}, {"cf", "route", "add", "d", "400"}} {
		wg.Go(func() {
			code, _, _, err := run(t, c, RunOptions{Argv: argv})
			assert.NoError(t, err)
			assert.Equal(t, 0, code)
		})
	}
	wg.Wait()
	cs, ce := span(t, e.stateDir, "c")
	ds, de := span(t, e.stateDir, "d")
	assert.True(t, ce <= ds || de <= cs, "cf route add ran unlocked: c=[%d,%d] d=[%d,%d]", cs, ce, ds, de)
}

func TestRemote_RefusesClientOnlyCommands(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")
	for _, argv := range [][]string{{"help", "up"}, {"completion", "bash"}, {"__complete", "u"}} {
		code, _, _, err := run(t, c, RunOptions{Argv: argv})
		require.Error(t, err, "%q", argv)
		assert.Equal(t, 2, code, "%q", argv)
	}
}

// A child that never reads stdin fills the queue; a client that floods it
// and then falls silent must still be dropped, its child killed and the
// lock released, within a few ping intervals.
func TestRemote_StalledStdinStillDropsSilentClient(t *testing.T) {
	e := newTestServer(t, func(c *ServerConfig) { c.PingInterval = 100 * time.Millisecond })
	tr := e.srv.Client().Transport.(*http.Transport)
	d := websocket.Dialer{TLSClientConfig: tr.TLSClientConfig.Clone()}
	url := "wss" + strings.TrimPrefix(e.srv.URL, "https") + Path
	conn, _, err := d.Dial(url, http.Header{"Authorization": {"Bearer " + Token(testSecret)}})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.WriteJSON(Handshake{Argv: []string{"up", "stall", "600000"}, Version: "1.4.2"}))
	pid := waitPid(t, e.stateDir, "stall")

	flooded := make(chan struct{})
	go func() { // well past the pipe buffer and the 16-frame queue; then silence
		defer close(flooded)
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for range 40 {
			if conn.WriteMessage(websocket.BinaryMessage, chunk) != nil {
				return
			}
		}
	}()

	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 10*time.Second, 20*time.Millisecond,
		"child of a silent client with a stalled stdin survived")
	<-flooded

	start := time.Now()
	code, _, _, err := run(t, e.client(testSecret, "1.4.2"), RunOptions{Argv: []string{"up", "next", "10"}})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Less(t, time.Since(start), 5*time.Second)
}
