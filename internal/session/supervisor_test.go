package session

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/docker"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type harness struct {
	sup    *Supervisor
	f      fixture
	dock   *fakeDocker
	run    *fakeRunner
	clock  *testClock
	logs   *bytes.Buffer
	sleeps []time.Duration
	mu     sync.Mutex
}

func newHarness(t *testing.T, desired *Desired, cs ...docker.ContainerInfo) *harness {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"), []byte("vars: {}\n"), 0o600))
	if desired != nil {
		require.NoError(t, desired.Save(root))
	}
	h := &harness{f: newFixture(root), dock: &fakeDocker{}, run: &fakeRunner{},
		clock: &testClock{now: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}, logs: &bytes.Buffer{}}
	for i := range cs {
		if cs[i].ID == "" {
			cs[i].ID = cs[i].Name + "-id"
		}
	}
	h.dock.cs = cs
	// docker update changes the fake's containers, as it would the daemon's.
	h.run.after = func(argv []string) {
		if len(argv) > 3 && argv[1] == "update" {
			pol := strings.TrimPrefix(argv[2], "--restart=")
			h.dock.mu.Lock()
			for i := range h.dock.cs {
				if slices.Contains(argv[3:], h.dock.cs[i].Name) {
					h.dock.cs[i].RestartPolicy = pol
				}
			}
			h.dock.mu.Unlock()
		}
	}
	h.sup = &Supervisor{
		Root:       root,
		ConfigFile: filepath.Join(root, "config.yaml"),
		Home:       "/home/u",
		CLI:        []string{"/bin/homelab", "--config-dir", root, "--no-record"},
		Docker:     h.dock,
		Run:        h.run.run,
		Log:        log.New(syncWriter{h}, "", 0),
		Now:        h.clock.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.mu.Lock()
			h.sleeps = append(h.sleeps, d)
			h.mu.Unlock()
			h.clock.Add(d)
			return ctx.Err()
		},
		Cutoff: func(time.Time) time.Time { return time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC) },
	}
	return h
}

type syncWriter struct{ h *harness }

func (w syncWriter) Write(p []byte) (int, error) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return w.h.logs.Write(p)
}

func (h *harness) Logs() string { h.mu.Lock(); defer h.mu.Unlock(); return h.logs.String() }

func running(svcs ...string) *Desired {
	d := &Desired{Core: Running, Services: map[string]State{}}
	for _, s := range svcs {
		d.Services[s] = Running
	}
	return d
}

func TestRestore_PoliciesRebindAndOrder(t *testing.T) {
	boot := time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC) // before the cutoff
	login := time.Date(2026, 10, 8, 8, 30, 0, 0, time.UTC)
	h := newHarness(t, nil)
	f := h.f
	h.dock.cs = []docker.ContainerInfo{
		{ID: "1", Name: "caddy", State: "exited", RestartPolicy: "always", Labels: f.core,
			BindSources: []string{"/home/u/.config/homelab/caddy/Caddyfile"}},
		{ID: "2", Name: "i2pd", State: "running", RestartPolicy: "always", Labels: f.core, StartedAt: boot,
			BindSources: []string{"/home/u/.config/homelab/i2p"}},
		{ID: "2b", Name: "tor", State: "running", RestartPolicy: "no", Labels: f.core, StartedAt: login},
		{ID: "3", Name: "web", State: "running", RestartPolicy: "no", Labels: f.svc("web"), StartedAt: login,
			BindSources: []string{"/home/u/.config/homelab/services/web/data"}},
		{ID: "4", Name: "homelab-postgres", State: "running", RestartPolicy: "unless-stopped", Labels: f.svc("postgres"),
			StartedAt: boot, BindSources: []string{"/var/lib/x"}},
		{ID: "5", Name: "stranger", State: "running", RestartPolicy: "always", Labels: labelsFor("/srv/x"), StartedAt: boot,
			BindSources: []string{"/home/u/y"}},
	}
	d := &Desired{Core: Running, Services: map[string]State{"web": Running, "postgres": Running, "off": Stopped}}
	require.NoError(t, d.Save(h.sup.Root))

	require.NoError(t, h.sup.Restore(context.Background(), h.sup.Cutoff(time.Time{})))

	cli := "/bin/homelab --config-dir " + h.sup.Root + " --no-record"
	assert.Equal(t, []string{
		"docker update --restart=no caddy homelab-postgres i2pd",
		"docker stop i2pd tor", // pre-login, binds under home, with the rest of its project; postgres binds nothing there, web started after login
		cli + " up",
		cli + " up postgres web", // shared databases first
	}, h.run.Calls())
}

func TestRestore_BootstrapsMissingDesiredState(t *testing.T) {
	h := newHarness(t, nil)
	h.dock.cs = []docker.ContainerInfo{{ID: "1", Name: "web", State: "running", RestartPolicy: "no", Labels: h.f.svc("web"),
		StartedAt: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}}
	require.NoError(t, h.sup.Restore(context.Background(), h.sup.Cutoff(time.Time{})))
	d, exists, err := LoadDesired(h.sup.Root)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.True(t, d.ServiceRunning("web"))
	assert.False(t, d.CoreRunning())
	assert.Equal(t, []string{"/bin/homelab --config-dir " + h.sup.Root + " --no-record up web"}, h.run.Calls())
}

func TestDecide(t *testing.T) {
	h := newHarness(t, &Desired{Core: Running, Services: map[string]State{"web": Running, "off": Stopped}})
	f := h.f
	require.NoError(t, Policies{"web": "unless-stopped", "job": "no", "flaky": "on-failure", "caddy": "always", "off": "always"}.Save(h.sup.Root))
	desired, _, _ := LoadDesired(h.sup.Root)
	desired.SetService("job", Running)
	desired.SetService("flaky", Running)
	require.NoError(t, desired.Save(h.sup.Root))

	s := h.sup
	assert.Equal(t, FixPolicy, s.Decide(ev("start", "w", "web", f.svc("web"))))
	assert.Equal(t, RestartCon, s.Decide(ev("die", "w", "web", f.svc("web"), "exitCode", "139")), "crash")
	assert.Equal(t, RestartCon, s.Decide(ev("die", "c", "caddy", f.core, "exitCode", "1")), "core crash")

	// docker stop / compose stop / backup: kill, then die.
	assert.Equal(t, Ignore, s.Decide(ev("kill", "w", "web", f.svc("web"), "signal", "15")))
	assert.Equal(t, Ignore, s.Decide(ev("die", "w", "web", f.svc("web"), "exitCode", "0")), "intentional stop")
	// The kill is consumed: the next die is a crash again.
	assert.Equal(t, RestartCon, s.Decide(ev("die", "w", "web", f.svc("web"), "exitCode", "1")))
	// A reload signal is not a stop.
	s.Decide(ev("kill", "w", "web", f.svc("web"), "signal", "1"))
	assert.Equal(t, RestartCon, s.Decide(ev("die", "w", "web", f.svc("web"), "exitCode", "1")), "after SIGHUP")
	// A kill long ago does not excuse a crash now.
	s.Decide(ev("kill", "w", "web", f.svc("web")))
	h.clock.Add(time.Hour)
	assert.Equal(t, RestartCon, s.Decide(ev("die", "w", "web", f.svc("web"), "exitCode", "1")))

	assert.Equal(t, Ignore, s.Decide(ev("die", "o", "off", f.svc("off"), "exitCode", "1")), "desired stopped (homelab stop)")
	assert.Equal(t, Ignore, s.Decide(ev("die", "j", "job", f.svc("job"), "exitCode", "0")), "restart: no — a one-shot")
	assert.Equal(t, Ignore, s.Decide(ev("die", "fl", "flaky", f.svc("flaky"), "exitCode", "0")), "on-failure, clean exit")
	assert.Equal(t, RestartCon, s.Decide(ev("die", "fl", "flaky", f.svc("flaky"), "exitCode", "2")), "on-failure, failed")

	oneoff := f.svc("web")
	oneoff["com.docker.compose.oneoff"] = "True"
	assert.Equal(t, Ignore, s.Decide(ev("die", "r", "web-run-1", oneoff, "exitCode", "1")), "compose run")
	assert.Equal(t, Ignore, s.Decide(ev("die", "x", "stranger", labelsFor("/srv/x"), "exitCode", "1")), "outside the config dir")
	assert.Equal(t, Ignore, s.Decide(ev("start", "x", "stranger", labelsFor("/srv/x"))))

	// The desired state is re-read on every event.
	d, _, _ := LoadDesired(h.sup.Root)
	d.SetService("web", Stopped)
	require.NoError(t, d.Save(h.sup.Root))
	assert.Equal(t, Ignore, s.Decide(ev("die", "w", "web", f.svc("web"), "exitCode", "1")))
}

func TestHandle_StartFixesPolicy(t *testing.T) {
	h := newHarness(t, running("web"), docker.ContainerInfo{Name: "web", State: "running", RestartPolicy: "always"})
	h.dock.cs[0].Labels = h.f.svc("web")
	h.sup.Handle(context.Background(), ev("start", "web-id", "web", h.f.svc("web")))
	assert.Equal(t, []string{"docker update --restart=no web"}, h.run.Calls())
	pol, _ := LoadPolicies(h.sup.Root)
	assert.Equal(t, "always", pol.Original("web"))
}

func TestHandle_RestartsCrashedContainer(t *testing.T) {
	h := newHarness(t, running("web"), docker.ContainerInfo{Name: "web", State: "exited", RestartPolicy: "no"})
	h.dock.cs[0].Labels = h.f.svc("web")
	require.NoError(t, Policies{"web": "always"}.Save(h.sup.Root))

	h.sup.Handle(context.Background(), ev("die", "web-id", "web", h.f.svc("web"), "exitCode", "1"))
	h.sup.wg.Wait()
	assert.Equal(t, []string{"docker start web-id"}, h.run.Calls())
	assert.Equal(t, []time.Duration{time.Second}, h.sleeps)
}

func TestHandle_SkipsWhenRecreatedOrStoppedMeanwhile(t *testing.T) {
	h := newHarness(t, running("web"), docker.ContainerInfo{Name: "web", State: "exited"})
	h.dock.cs[0].Labels = h.f.svc("web")
	require.NoError(t, Policies{"web": "always"}.Save(h.sup.Root))

	// Already running again (compose restart, or someone else): nothing to do.
	h.dock.setState("web-id", "running")
	h.sup.Handle(context.Background(), ev("die", "web-id", "web", h.f.svc("web"), "exitCode", "1"))
	h.sup.wg.Wait()
	// Gone (compose recreated it).
	h.sup.Handle(context.Background(), ev("die", "old-id", "web", h.f.svc("web"), "exitCode", "1"))
	h.sup.wg.Wait()
	assert.Empty(t, h.run.Calls())
}

func TestHandle_BackoffAndGiveUp(t *testing.T) {
	h := newHarness(t, running("web"), docker.ContainerInfo{Name: "web", State: "exited"})
	h.dock.cs[0].Labels = h.f.svc("web")
	require.NoError(t, Policies{"web": "always"}.Save(h.sup.Root))
	h.run.fail = func([]string) error { return errFake } // docker start keeps failing

	h.sup.Handle(context.Background(), ev("die", "web-id", "web", h.f.svc("web"), "exitCode", "1"))
	h.sup.wg.Wait()
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}, h.sleeps)
	assert.Len(t, h.run.Calls(), 5)
	assert.Contains(t, h.Logs(), "!!! web: gave up")

	// Further deaths within the window are not retried either.
	h.sup.Handle(context.Background(), ev("die", "web-id", "web", h.f.svc("web"), "exitCode", "1"))
	h.sup.wg.Wait()
	assert.Len(t, h.run.Calls(), 5)
}

func TestShutdown_ServicesThenCore(t *testing.T) {
	h := newHarness(t, running("web"))
	f := h.f
	h.dock.cs = []docker.ContainerInfo{
		{Name: "caddy", State: "running", Labels: f.core},
		{Name: "homelab-redis", State: "running", Labels: f.svc("redis")},
		{Name: "web", State: "running", Labels: f.svc("web")},
		{Name: "old", State: "exited", Labels: f.svc("old")},
		{Name: "stranger", State: "running", Labels: labelsFor("/srv/x")},
	}
	require.NoError(t, h.sup.Shutdown(context.Background()))
	assert.Equal(t, []string{"docker stop web homelab-redis", "docker stop caddy"}, h.run.Calls())

	d, _, _ := LoadDesired(h.sup.Root)
	assert.True(t, d.ServiceRunning("web"), "logout must not rewrite what the next login restores")
	assert.True(t, d.CoreRunning())
}

func TestShutdown_DetachLeavesStackRunning(t *testing.T) {
	h := newHarness(t, running())
	h.dock.cs = []docker.ContainerInfo{{Name: "caddy", State: "running", Labels: h.f.core}}
	require.NoError(t, os.MkdirAll(StateDir(h.sup.Root), 0o700))
	require.NoError(t, os.WriteFile(DetachFile(h.sup.Root), nil, 0o600))
	require.NoError(t, h.sup.Shutdown(context.Background()))
	assert.Empty(t, h.run.Calls())
}

func TestServe_WaitsForConfigThenRestoresAndStops(t *testing.T) {
	h := newHarness(t, running())
	h.dock.cs = []docker.ContainerInfo{{Name: "caddy", State: "exited", Labels: h.f.core}}
	require.NoError(t, os.Remove(h.sup.ConfigFile)) // home not mounted yet

	ctx, cancel := context.WithCancel(context.Background())
	var readySince time.Time
	h.sup.Cutoff = func(r time.Time) time.Time { readySince = r; return r }
	polls := 0
	h.sup.Sleep = func(ctx context.Context, _ time.Duration) error {
		polls++
		if polls == 2 { // "login": the config dir appears
			_ = os.WriteFile(h.sup.ConfigFile, []byte("vars: {}\n"), 0o600)
		}
		return ctx.Err()
	}
	h.run.after = func(argv []string) {
		if argv[len(argv)-1] == "up" && argv[0] != "docker" { // core restored: now log out
			h.dock.setState("caddy-id", "running")
			h.dock.mu.Lock()
			h.dock.cs[0].State = "running"
			h.dock.mu.Unlock()
			cancel()
		}
	}
	require.NoError(t, h.sup.Serve(ctx, time.Minute))
	assert.False(t, readySince.IsZero(), "waited for the config dir")
	assert.Equal(t, []string{
		"/bin/homelab --config-dir " + h.sup.Root + " --no-record up",
		"docker stop caddy",
	}, h.run.Calls())
	assert.Contains(t, h.Logs(), "waiting for")
}

// A login restore can race the shared databases coming up; failed service
// starts are retried with the restoreRetries waits instead of given up on.
func TestRestore_RetriesServicesThatFailToStart(t *testing.T) {
	h := newHarness(t, running("bifrost"))
	fails := 2
	h.run.fail = func(argv []string) error {
		if argv[len(argv)-1] == "bifrost" && fails > 0 {
			fails--
			return errors.New("provisioning postgres: connection refused")
		}
		return nil
	}
	require.NoError(t, h.sup.Restore(context.Background(), h.sup.Cutoff(time.Time{})))

	cli := "/bin/homelab --config-dir " + h.sup.Root + " --no-record"
	ups := 0
	for _, c := range h.run.Calls() {
		if c == cli+" up bifrost" {
			ups++
		}
	}
	assert.Equal(t, 3, ups, "two failures, then success")
	assert.Equal(t, restoreRetries[:2], h.sleeps)
	assert.Contains(t, h.Logs(), "retrying in 10s")
	assert.NotContains(t, h.Logs(), "giving up")
}
