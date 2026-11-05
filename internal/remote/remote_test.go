package remote

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain doubles as the child the server runs: the server execs
// os.Args[0] (this test binary) with HOMELAB_AGENT=1 and
// `--config-dir <dir> [--no-color] <argv…>`, and helperMain plays homelab.
func TestMain(m *testing.M) {
	if os.Getenv("HOMELAB_AGENT") == "1" {
		helperMain(os.Args[1:])
	}
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-test-docker.sock")
	os.Exit(m.Run())
}

// helperMain: args are `--config-dir <dir> [--no-color] <verb> <args…>`.
func helperMain(args []string) {
	if len(args) < 3 || args[0] != "--config-dir" {
		fmt.Fprintf(os.Stderr, "helper: bad argv %q\n", args)
		os.Exit(99)
	}
	dir := args[1]
	args = args[2:]
	if args[0] == "--no-color" {
		args = args[1:]
	}
	switch args[0] {
	case "status": // prints, writes stderr, exits 3
		fmt.Println("hello from the agent")
		fmt.Fprintln(os.Stderr, "a warning")
		os.Exit(3)
	case "ls": // dumps the env it was exec'd with (not what package inits set since) and argv
		raw, _ := os.ReadFile("/proc/self/environ")
		env := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		sort.Strings(env)
		fmt.Println(strings.Join(env, "\n"))
		fmt.Println("ARGV=" + strings.Join(os.Args[1:], " "))
		os.Exit(0)
	case "cat": // echoes stdin
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(os.Stdin)
		fmt.Print("got:" + buf.String())
		os.Exit(0)
	case "up": // up <id> <sleep-ms>: records its interval in dir
		id := args[1]
		ms, _ := strconv.Atoi(args[2])
		_ = os.WriteFile(filepath.Join(dir, id+".pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		start := time.Now().UnixNano()
		time.Sleep(time.Duration(ms) * time.Millisecond)
		end := time.Now().UnixNano()
		_ = os.WriteFile(filepath.Join(dir, id+".span"), fmt.Appendf(nil, "%d %d", start, end), 0o600)
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "helper: unknown verb %q\n", args[0])
	os.Exit(98)
}

const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type testEnv struct {
	srv      *httptest.Server
	stateDir string
	logs     *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) logf(format string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(&s.b, format+"\n", a...)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestServer(t *testing.T, mod func(*ServerConfig)) *testEnv {
	t.Helper()
	root := t.TempDir()
	logs := &syncBuf{}
	cfg := ServerConfig{
		StateDir:   filepath.Join(root, "state"),
		StateRoot:  root,
		Secret:     testSecret,
		Version:    "1.4.2",
		Binary:     os.Args[0],
		Vault:      filepath.Join(root, "vault.bin"),
		VaultKey:   filepath.Join(root, "vault.key"),
		CaddyAdmin: "127.0.0.1:2019",
		KillGrace:  2 * time.Second,
		Logf:       logs.logf,
	}
	if mod != nil {
		mod(&cfg)
	}
	require.NoError(t, os.MkdirAll(cfg.StateDir, 0o700))
	srv := httptest.NewTLSServer(NewServer(cfg).Handler())
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, stateDir: cfg.StateDir, logs: logs}
}

func (e *testEnv) client(secret, version string) *Client {
	tr := e.srv.Client().Transport.(*http.Transport)
	return &Client{Endpoint: e.srv.URL, Secret: secret, Version: version, TLSConfig: tr.TLSClientConfig.Clone()}
}

func run(t *testing.T, c *Client, opts RunOptions) (int, string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	opts.Stdout, opts.Stderr = &out, &errb
	if opts.Stdin == nil {
		opts.Stdin = strings.NewReader("")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, err := c.Run(ctx, opts)
	return code, out.String(), errb.String(), err
}

func TestToken_IsHMACOfContext(t *testing.T) {
	tok := Token(testSecret)
	assert.Len(t, tok, 64)
	assert.NotContains(t, tok, testSecret)
	assert.Equal(t, tok, Token(testSecret))
	assert.NotEqual(t, tok, Token(testSecret+"x"))
}

func TestRemote_RejectsBadToken(t *testing.T) {
	e := newTestServer(t, nil)

	// Raw request: 401 with no detail.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/run", nil)
	req.Header.Set("Authorization", "Bearer "+Token("wrong"))
	resp, err := e.srv.Client().Do(req)
	require.NoError(t, err)
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, strings.TrimSpace(body.String()))

	// No header at all.
	resp, err = e.srv.Client().Get(e.srv.URL + "/v1/run")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Through the client: an error naming the cause, never a secret.
	code, _, _, err := run(t, e.client("not-the-secret", "1.4.2"), RunOptions{Argv: []string{"status"}})
	require.Error(t, err)
	assert.NotEqual(t, 0, code)
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), "not-the-secret")
	assert.NotContains(t, err.Error(), Token("not-the-secret"))

	logs := e.logs.String()
	assert.Contains(t, logs, "127.0.0.1")
	assert.NotContains(t, logs, testSecret)
	assert.NotContains(t, logs, Token(testSecret))
	assert.NotContains(t, logs, Token("wrong"))
}

func TestRemote_StreamsOutputAndExitCode(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.7")

	t.Run("pipes", func(t *testing.T) {
		code, out, errOut, err := run(t, c, RunOptions{Argv: []string{"status"}})
		require.NoError(t, err)
		assert.Equal(t, 3, code)
		assert.Equal(t, "hello from the agent\n", out)
		assert.Equal(t, "a warning\n", errOut)
	})
	t.Run("pty", func(t *testing.T) {
		code, out, _, err := run(t, c, RunOptions{Argv: []string{"status"}, TTY: true, Rows: 40, Cols: 100, Term: "xterm-256color"})
		require.NoError(t, err)
		assert.Equal(t, 3, code)
		assert.Contains(t, out, "hello from the agent")
		assert.Contains(t, out, "a warning") // one terminal carries both
	})
	t.Run("stdin", func(t *testing.T) {
		code, out, _, err := run(t, c, RunOptions{Argv: []string{"cat"}, Stdin: strings.NewReader("secret-json")})
		require.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, "got:secret-json", out)
	})
}

func TestRemote_RejectsConfigDirInArgv(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")
	for _, argv := range [][]string{
		{"ls", "--config-dir", "/etc"},
		{"ls", "--config-dir=/etc"},
		{"--config", "/tmp/x.yaml", "ls"},
		{"ls", "--config=/tmp/x.yaml"},
		{},
		{"version"},
		{"login"},
		{"--no-color", "join"},
		{"--gui"}, // the root command renders locally
	} {
		code, out, _, err := run(t, c, RunOptions{Argv: argv})
		require.Error(t, err, "%q", argv)
		assert.NotEqual(t, 0, code, "%q", argv)
		assert.NotContains(t, out, "ARGV=", "%q ran", argv)
	}
}

func TestRemote_ChildEnvIsAllowlisted(t *testing.T) {
	t.Setenv("HL_LEAK_CANARY", "leaked")
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")

	_, out, _, err := run(t, c, RunOptions{Argv: []string{"ls", "-q"}})
	require.NoError(t, err)
	var keys []string
	vals := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if a, ok := strings.CutPrefix(line, "ARGV="); ok {
			assert.Equal(t, "--config-dir "+e.stateDir+" --no-color ls -q", a)
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		keys = append(keys, k)
		vals[k] = v
	}
	assert.ElementsMatch(t, []string{"HOMELAB_AGENT", "HOMELAB_VAULT", "HOMELAB_VAULT_KEY",
		"HOMELAB_CADDY_ADMIN", "PATH", "HOME", "TERM", "LANG"}, keys)
	assert.Equal(t, "1", vals["HOMELAB_AGENT"])
	assert.Equal(t, filepath.Dir(e.stateDir), vals["HOME"])
	assert.Equal(t, "dumb", vals["TERM"])
	assert.Equal(t, "C.UTF-8", vals["LANG"])
	assert.Equal(t, "127.0.0.1:2019", vals["HOMELAB_CADDY_ADMIN"])

	// With a tty, TERM comes from the client and --no-color is not passed.
	_, out, _, err = run(t, c, RunOptions{Argv: []string{"ls"}, TTY: true, Rows: 24, Cols: 80, Term: "xterm-kitty"})
	require.NoError(t, err)
	assert.Contains(t, out, "TERM=xterm-kitty")
	assert.Contains(t, out, "ARGV=--config-dir "+e.stateDir+" ls")
}

func span(t *testing.T, dir, id string) (int64, int64) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, id+".span"))
	require.NoError(t, err)
	f := strings.Fields(string(b))
	s, _ := strconv.ParseInt(f[0], 10, 64)
	en, _ := strconv.ParseInt(f[1], 10, 64)
	return s, en
}

func TestRunner_MutatingCommandsSerialise(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")

	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Go(func() {
			code, _, _, err := run(t, c, RunOptions{Argv: []string{"up", id, "400"}})
			assert.NoError(t, err)
			assert.Equal(t, 0, code)
		})
	}
	// A read-only command does not wait for the lock.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	code, _, _, err := run(t, c, RunOptions{Argv: []string{"status"}})
	require.NoError(t, err)
	assert.Equal(t, 3, code)
	assert.Less(t, time.Since(start), 350*time.Millisecond, "read-only command waited on the lock")
	wg.Wait()

	as, ae := span(t, e.stateDir, "a")
	bs, be := span(t, e.stateDir, "b")
	assert.True(t, ae <= bs || be <= as, "mutating commands overlapped: a=[%d,%d] b=[%d,%d]", as, ae, bs, be)
}

func TestRemote_DisconnectKillsChildAndReleasesLock(t *testing.T) {
	e := newTestServer(t, nil)
	c := e.client(testSecret, "1.4.2")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Run(ctx, RunOptions{Argv: []string{"up", "hang", "600000"}, Stdin: strings.NewReader(""),
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	}()
	pidFile := filepath.Join(e.stateDir, "hang.pid")
	require.Eventually(t, func() bool { _, err := os.Stat(pidFile); return err == nil }, 10*time.Second, 20*time.Millisecond)
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(string(b))
	require.NotZero(t, pid)

	cancel() // the client goes away
	<-done

	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 10*time.Second, 20*time.Millisecond,
		"child survived the disconnect")

	// The lock is free again: the next mutating command runs promptly.
	start := time.Now()
	code, _, _, err := run(t, c, RunOptions{Argv: []string{"up", "next", "10"}})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestClient_RefusesVersionMismatch(t *testing.T) {
	e := newTestServer(t, nil)

	code, out, _, err := run(t, e.client(testSecret, "1.5.0"), RunOptions{Argv: []string{"ls"}})
	require.Error(t, err)
	assert.Equal(t, 2, code)
	assert.Contains(t, err.Error(), "homelab CLI 1.5.0 and agent 1.4.2 differ — run `homelab upgrade`")
	assert.NotContains(t, out, "ARGV=")

	// A patch difference is fine.
	code, _, _, err = run(t, e.client(testSecret, "v1.4.9"), RunOptions{Argv: []string{"ls"}})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
}

func TestLoadClient(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	_, err := LoadClient("1.0.0", func() (string, error) { return testSecret, nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "homelab login")
	assert.Contains(t, err.Error(), "homelab join")

	require.NoError(t, os.MkdirAll(filepath.Join(xdg, "homelab"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(xdg, "homelab", "client.yaml"),
		[]byte("endpoint: https://homelab.tail1234.ts.net:7443\n"), 0o600))
	c, err := LoadClient("1.0.0", func() (string, error) { return testSecret, nil })
	require.NoError(t, err)
	assert.Equal(t, "https://homelab.tail1234.ts.net:7443", c.Endpoint)
	assert.Equal(t, testSecret, c.Secret)
	assert.Nil(t, c.TLSConfig, "production clients verify against the system roots")

	_, err = LoadClient("1.0.0", func() (string, error) { return "", nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CLUSTER_SECRET")
}

