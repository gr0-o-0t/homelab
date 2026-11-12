package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/edge"
	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

func TestMain(m *testing.M) {
	// Nothing here may reach a real daemon.
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-agent-test.sock")
	os.Exit(m.Run())
}

// recorder is the shared, ordered log of what the fakes were asked to do.
type recorder struct {
	mu    sync.Mutex
	calls []string
	logs  []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recorder) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// waitFor blocks until call has been recorded: the rebalancer is a
// goroutine, so a later step waits for it rather than racing it.
func (r *recorder) waitFor(call string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range r.Calls() {
			if c == call {
				return true
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recorder) Logs() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

type fakeDeployer struct {
	rec  *recorder
	fail map[string]error
	// replicas is each service's Swarm services and their counts; a
	// service missing here has none (never deployed).
	replicas map[string]map[string]uint64
	onUp     func(svc string)
	// upCtx, when set, replaces Up's result: it gets Up's ctx.
	upCtx func(ctx context.Context, svc string) error
	// replicasCtx, when set, runs first in Replicas.
	replicasCtx func(ctx context.Context, svc string) error
}

func (d *fakeDeployer) Replicas(ctx context.Context, svc string) (map[string]uint64, error) {
	if d.replicasCtx != nil {
		if err := d.replicasCtx(ctx, svc); err != nil {
			return nil, err
		}
	}
	return maps.Clone(d.replicas[svc]), nil
}

func (d *fakeDeployer) Scale(_ context.Context, svc string, r map[string]uint64) error {
	for _, k := range slices.Sorted(maps.Keys(r)) {
		d.rec.add(fmt.Sprintf("scale %s %s=%d", svc, k, r[k]))
	}
	return nil
}

func (d *fakeDeployer) EnsureNetwork(context.Context) error { d.rec.add("network"); return nil }
func (d *fakeDeployer) EnsureCloudflared(_ context.Context, tok string) error {
	d.rec.add("cloudflared " + tok)
	return nil
}
func (d *fakeDeployer) RemoveCloudflared(context.Context) error {
	d.rec.add("remove-cloudflared")
	return nil
}
func (d *fakeDeployer) Up(ctx context.Context, svc string) error {
	if d.upCtx != nil {
		if err := d.upCtx(ctx, svc); err != nil {
			return err
		}
	}
	if d.onUp != nil {
		d.onUp(svc)
	}
	d.rec.add("up " + svc)
	return d.fail[svc]
}

type fakeEdge struct {
	rec     *recorder
	shim    string
	ip      netip.Addr
	authKey string
	// startErrs are returned by successive Starts; then nil.
	startErrs []error
}

func (e *fakeEdge) Start(context.Context) error {
	if len(e.startErrs) > 0 {
		err := e.startErrs[0]
		e.startErrs = e.startErrs[1:]
		e.rec.add("edge-start-failed")
		if !errors.Is(err, edge.ErrConfigNotLoaded) {
			return err
		}
		e.rec.add("edge") // up, admin only
		return err
	}
	if _, err := os.Stat(e.shim); err == nil {
		e.rec.add("shim")
	}
	e.rec.add("edge")
	return nil
}
func (e *fakeEdge) Stop() error             { e.rec.add("edge-stop"); return nil }
func (e *fakeEdge) TailnetIPv4() netip.Addr { return e.ip }
func (e *fakeEdge) SOCKS() (string, string, error) {
	return "127.0.0.1:1080", "socks-cred-SECRET", nil
}
func (e *fakeEdge) ListenTLS(network, addr string) (net.Listener, error) {
	e.rec.add("listen " + addr)
	return net.Listen("tcp", "127.0.0.1:0")
}

type fakeNode struct {
	rec      *recorder
	hostname string
}

func (n *fakeNode) ListenTLS(network, addr string) (net.Listener, error) {
	n.rec.add("listen " + addr)
	return net.Listen("tcp", "127.0.0.1:0")
}
func (n *fakeNode) Close() error { n.rec.add("node-close"); return nil }

// harness is an agent on fakes, with a vault in a temp root.
type harness struct {
	t       *testing.T
	a       *Agent
	rec     *recorder
	dep     *fakeDeployer
	edge    *fakeEdge
	serving chan remote.ServerConfig
	sm      *secrets.Manager
	paths   agentstate.Paths
	role    Role

	mu  sync.Mutex
	cfg remote.ServerConfig // what Serve was given
}

func (h *harness) served() remote.ServerConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

func newHarness(t *testing.T, role Role) *harness {
	t.Helper()
	root := t.TempDir()
	paths := agentstate.At(root)
	sm, err := secrets.OpenVault(paths.VaultFile, paths.KeyFile)
	require.NoError(t, err)
	require.NoError(t, agentstate.SaveCluster(paths, sm, agentstate.Cluster{Secret: "s3cret", TSHostname: "homelab"}))

	bin := filepath.Join(t.TempDir(), "homelab-bin")
	require.NoError(t, os.WriteFile(bin, []byte("#!binary v1"), 0o755)) //nolint:gosec // test binary
	shimDir := filepath.Join(t.TempDir(), "hl")
	require.NoError(t, os.Mkdir(shimDir, 0o755)) //nolint:gosec // the mounted volume

	rec := &recorder{}
	h := &harness{t: t, rec: rec, sm: sm, paths: paths, role: role, serving: make(chan remote.ServerConfig, 1)}
	h.dep = &fakeDeployer{rec: rec, fail: map[string]error{}}
	h.edge = &fakeEdge{rec: rec, shim: filepath.Join(shimDir, ShimName), ip: netip.MustParseAddr("100.101.102.103")}
	h.a = &Agent{
		Root:    root,
		ShimDir: shimDir,
		Binary:  bin,
		Version: "1.2.3",
		Resolve: func(argv []string) ([]string, bool) {
			if len(argv) == 0 {
				return nil, false
			}
			if argv[0] == "agent" && len(argv) > 1 {
				return argv[:2], true
			}
			return []string{argv[0]}, true
		},
		Inspect: func(context.Context) (NodeInfo, error) { return NodeInfo{Role: role, Hostname: "laptop"}, nil },
		NewEdge: func(authKey string, _ func() (map[string]string, error)) Edge {
			h.edge.authKey = authKey
			return h.edge
		},
		NewNode: func(_ context.Context, hostname, dir, _ string) (Node, error) {
			rec.add("node " + hostname + " " + dir)
			return &fakeNode{rec: rec, hostname: hostname}, nil
		},
		NewDeployer: func(*secrets.Manager) Deployer { return h.dep },
		EnsureDNS: func(_ context.Context, token, zone, name string, ip netip.Addr) error {
			rec.add(fmt.Sprintf("dns %s %s %s %s", token, zone, name, ip))
			return nil
		},
		Rebalance: func(ctx context.Context) error {
			rec.add("rebalance")
			<-ctx.Done()
			rec.add("rebalance-stopped")
			return nil
		},
		Serve: func(ln net.Listener, cfg remote.ServerConfig) error {
			h.mu.Lock()
			h.cfg = cfg
			h.mu.Unlock()
			rec.add("serve")
			h.serving <- cfg
			for {
				c, err := ln.Accept()
				if err != nil {
					rec.add("serve-stopped")
					return err
				}
				_ = c.Close()
			}
		},
		Sleep: func(ctx context.Context, d time.Duration) bool {
			rec.add("sleep " + d.String())
			return ctx.Err() == nil
		},
		Logf: rec.logf,
	}
	return h
}

func (h *harness) writeConfig(vars map[string]string) {
	h.t.Helper()
	var b strings.Builder
	b.WriteString("vars:\n")
	for k, v := range vars {
		fmt.Fprintf(&b, "  %s:\n    value: %q\n", k, v)
	}
	require.NoError(h.t, os.MkdirAll(h.paths.StateDir, 0o700))
	require.NoError(h.t, os.WriteFile(filepath.Join(h.paths.StateDir, "config.yaml"), []byte(b.String()), 0o600))
}

func (h *harness) writeDesired(svcs map[string]desired.State) {
	h.t.Helper()
	for name := range svcs {
		require.NoError(h.t, os.MkdirAll(filepath.Join(h.paths.StateDir, "services", name), 0o700))
	}
	d := &desired.Desired{Services: svcs}
	require.NoError(h.t, d.Save(h.paths.StateDir))
}

// start runs the agent until it serves — and, on the leader, has brought
// the services up and started the rebalancer, which follow serving — the
// returned func cancels it (the SIGTERM path) and waits for Run to return.
func (h *harness) start() (remote.ServerConfig, func() error) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.a.Run(ctx) }()
	select {
	case cfg := <-h.serving:
		if h.role == RoleLeader && !h.rec.waitFor("rebalance") {
			cancel()
			h.t.Fatalf("the leader never started its rebalancer; calls %v\n%s", h.rec.Calls(), h.rec.Logs())
		}
		return cfg, func() error { cancel(); return <-done }
	case err := <-done:
		cancel()
		h.t.Fatalf("Run returned before serving: %v\n%s", err, h.rec.Logs())
	case <-time.After(10 * time.Second):
		cancel()
		h.t.Fatalf("agent never served; calls %v\n%s", h.rec.Calls(), h.rec.Logs())
	}
	return remote.ServerConfig{}, nil
}

func TestAgent_LeaderStartupOrder(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeConfig(map[string]string{"DOMAIN": "example.test", "HOME_SUBDOMAIN": "lab"})
	require.NoError(t, h.sm.Set("", "CLOUDFLARE_API_TOKEN", "cftok"))
	require.NoError(t, h.sm.Set("", "CF_TUNNEL_TOKEN", "tunneltok"))
	require.NoError(t, h.sm.Set("", "TS_AUTHKEY", "tskey"))
	h.writeDesired(map[string]desired.State{
		"zeta": desired.Running, "alpha": desired.Running, "redis": desired.Running,
		"postgres": desired.Running, "off": desired.Stopped,
	})
	// The remote CLI answers while services come up (R53): bringUp runs
	// once serving has begun, holding the server's lock so a mutating
	// command waits for it while reads (status) go ahead.
	var lockHeld []bool
	h.dep.onUp = func(string) {
		assert.True(t, h.rec.waitFor("serve"), "services are brought up only once the remote CLI serves")
		cfg := h.served()
		lockHeld = append(lockHeld, len(cfg.Lock) == 1)
	}

	cfg, stop := h.start()
	require.NoError(t, stop())

	assert.Equal(t, []string{
		"network",
		"shim",
		"edge",
		"dns cftok example.test *.lab.example.test 100.101.102.103",
		"cloudflared tunneltok",
		"listen :7443",
		"serve",
		"up postgres", "up redis", // shared providers first
		"up alpha", "up zeta",
		"rebalance",
		"serve-stopped", "rebalance-stopped", "edge-stop",
	}, h.rec.Calls())
	assert.Equal(t, []bool{true, true, true, true}, lockHeld, "bringUp holds the server's lock")
	require.NotNil(t, cfg.Lock)
	assert.Empty(t, cfg.Lock, "released once the services are up")
	assert.Equal(t, "tskey", h.edge.authKey)

	// The remote endpoint runs this binary against the agent's state.
	assert.Equal(t, h.paths.StateDir, cfg.StateDir)
	assert.Equal(t, h.a.Root, cfg.StateRoot)
	assert.Equal(t, "s3cret", cfg.Secret)
	assert.Equal(t, "1.2.3", cfg.Version)
	assert.Equal(t, h.a.Binary, cfg.Binary)
	assert.Equal(t, h.paths.VaultFile, cfg.Vault)
	assert.Equal(t, h.paths.KeyFile, cfg.VaultKey)
	// Children relay exec/logs to workers through the edge's tailnet proxy.
	assert.Equal(t, "127.0.0.1:1080", cfg.TSSocks)
	assert.Equal(t, "socks-cred-SECRET", cfg.TSSocksCred)
	assert.NotContains(t, h.rec.Logs(), "socks-cred-SECRET")
	assert.Equal(t, filepath.Join(h.paths.StateDir, "caddy-admin.sock"), cfg.CaddyAdmin)
	require.NotNil(t, cfg.Resolve)
	path, ok := cfg.Resolve([]string{"up", "x"})
	assert.True(t, ok, "the leader serves every command")
	assert.Equal(t, []string{"up"}, path)
	assert.Empty(t, cfg.AllowInternal, "the leader replaces itself only as upgrade's last step, never on request")
}

// R53: a reboot (or an agent restart) does not redeploy what Swarm already
// runs — that would pull and re-resolve floating tags. Only a service
// recorded as running with no Swarm services is deployed, and one an
// interrupted backup left at zero replicas is scaled back, nothing else.
func TestAgent_BootDeploysOnlyMissingServices(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeDesired(map[string]desired.State{
		"alpha": desired.Running, "beta": desired.Running, "gamma": desired.Running, "off": desired.Stopped,
	})
	h.dep.replicas = map[string]map[string]uint64{
		"alpha": {"alpha_alpha": 1},
		"gamma": {"gamma_gamma": 1, "gamma_worker": 0},
		"off":   {"off_off": 0},
	}
	_, stop := h.start()
	require.NoError(t, stop())
	calls := h.rec.Calls()
	assert.Contains(t, calls, "up beta")
	assert.Contains(t, calls, "scale gamma gamma_worker=1")
	for _, c := range calls {
		assert.NotEqual(t, "up alpha", c, "alpha is in Swarm: left as Swarm keeps it")
		assert.NotEqual(t, "up gamma", c)
		assert.False(t, strings.HasPrefix(c, "scale gamma gamma_gamma"), c)
		assert.False(t, strings.HasPrefix(c, "scale off") || c == "up off", c)
	}
}

// The same against the real Deployer on a fake engine: booting with a
// service already in Swarm makes no ServiceUpdate and pulls nothing; a
// missing one is created from the local image.
func TestAgent_BootLeavesSwarmServicesAlone(t *testing.T) {
	h := newHarness(t, RoleLeader)
	f := swarmtest.New()
	f.SetImage("nginx:1", image.InspectResponse{})
	dep := &swarm.Deployer{API: f, Root: h.paths.StateDir, SM: h.sm}
	h.a.NewDeployer = func(*secrets.Manager) Deployer { return dep }
	for _, svc := range []string{"web", "api"} {
		dir := filepath.Join(h.paths.StateDir, "services", svc)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(fmt.Sprintf(`
networks:
  home-services:
    name: home-services
    external: true
services:
  %s:
    image: nginx:1
    container_name: %s
    networks: [home-services]
`, svc, svc)), 0o600))
	}
	require.NoError(t, dep.Up(context.Background(), "web"))
	before := len(f.CallLog())
	h.writeDesired(map[string]desired.State{"web": desired.Running, "api": desired.Running})

	_, stop := h.start()
	require.NoError(t, stop())
	log := f.CallLog()[before:]
	for _, c := range log {
		assert.False(t, strings.HasPrefix(c, "ServiceUpdate"), "%s: web is already in Swarm", c)
		assert.False(t, strings.HasPrefix(c, "ImagePull"), "%s: nothing is pulled at boot", c)
	}
	assert.Contains(t, log, "ServiceCreate api_api")
	assert.NotContains(t, f.RegistryQueries(), true)
}

// Without CF_TUNNEL_TOKEN the tunnel connector is removed, not left running
// on a stale token.
func TestAgent_LeaderRemovesCloudflaredWithoutToken(t *testing.T) {
	h := newHarness(t, RoleLeader)
	_, stop := h.start()
	require.NoError(t, stop())
	assert.Contains(t, h.rec.Calls(), "remove-cloudflared")
	for _, c := range h.rec.Calls() {
		assert.False(t, strings.HasPrefix(c, "cloudflared "), c)
	}
}

func TestAgent_WildcardRecordFollowsTailnetIP(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeConfig(map[string]string{"DOMAIN": "example.test"})
	require.NoError(t, h.sm.Set("", "CLOUDFLARE_API_TOKEN", "cftok"))
	h.edge.ip = netip.MustParseAddr("100.64.9.9")
	_, stop := h.start()
	require.NoError(t, stop())
	assert.Contains(t, h.rec.Calls(), "dns cftok example.test *.home.example.test 100.64.9.9",
		"HOME_SUBDOMAIN defaults to home; the record follows the node's IPv4")
}

// No token (or no domain) is a warning: tailnet access by IP still works, so
// startup carries on.
func TestAgent_DNSWithoutTokenWarnsAndContinues(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeConfig(map[string]string{"DOMAIN": "example.test"})
	h.a.EnsureDNS = func(context.Context, string, string, string, netip.Addr) error {
		t.Error("no DNS change without a token")
		return nil
	}
	_, stop := h.start()
	require.NoError(t, stop())
	assert.Contains(t, h.rec.Logs(), "CLOUDFLARE_API_TOKEN or DOMAIN not set")

	h2 := newHarness(t, RoleLeader)
	h2.writeConfig(map[string]string{"DOMAIN": "example.test"})
	require.NoError(t, h2.sm.Set("", "CLOUDFLARE_API_TOKEN", "cftok"))
	h2.a.EnsureDNS = func(context.Context, string, string, string, netip.Addr) error {
		return errors.New("zone example.test not visible")
	}
	_, stop2 := h2.start()
	require.NoError(t, stop2())
	assert.Contains(t, h2.rec.Logs(), "zone example.test not visible")
}

func TestAgent_StartupSurvivesAFailingService(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeDesired(map[string]desired.State{"alpha": desired.Running, "beta": desired.Running, "postgres": desired.Running})
	h.dep.fail["postgres"] = errors.New("image pull failed")
	h.dep.fail["alpha"] = errors.New("compose file broken")
	_, stop := h.start()
	require.NoError(t, stop())
	calls := h.rec.Calls()
	assert.Subset(t, calls, []string{"up postgres", "up alpha", "up beta", "serve"})
	assert.Contains(t, h.rec.Logs(), "bringing up alpha: compose file broken")
	assert.Contains(t, h.rec.Logs(), "bringing up postgres: image pull failed")
}

// SIGTERM stops serving, the rebalancer and the edge — and nothing else: no
// service is scaled down or removed, Swarm keeps them.
func TestAgent_SIGTERMNeverScalesDown(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeDesired(map[string]desired.State{"alpha": desired.Running})
	_, stop := h.start()
	before := len(h.rec.Calls())
	require.NoError(t, stop())
	assert.Equal(t, []string{"serve-stopped", "rebalance-stopped", "edge-stop"}, h.rec.Calls()[before:])
}

func TestAgent_WorkerServesOnlyLocalExec(t *testing.T) {
	h := newHarness(t, RoleWorker)
	h.writeDesired(map[string]desired.State{"alpha": desired.Running})
	cfg, stop := h.start()
	require.NoError(t, stop())

	calls := h.rec.Calls()
	assert.Equal(t, []string{
		"node homelab-laptop " + filepath.Join(h.a.Root, "tsnet"),
		"listen :7443",
		"serve",
		"serve-stopped",
		"node-close",
	}, calls, "a worker runs no edge, deploys nothing and rebalances nothing")
	assert.FileExists(t, filepath.Join(h.a.ShimDir, ShimName), "workers keep their shim volume current too")
	assert.Equal(t, "s3cret", cfg.Secret)
	assert.Empty(t, cfg.CaddyAdmin)
	assert.Empty(t, cfg.TSSocks, "a worker relays nothing")

	for _, argv := range [][]string{{"exec", "web", "sh"}, {"logs", "web"}, {"version", "--agent"}, {"version"}} {
		path, ok := cfg.Resolve(argv)
		assert.True(t, ok, "%v", argv)
		assert.Equal(t, argv[:1], path)
	}
	// upgrade replaces a worker's agent through the worker itself.
	path, ok := cfg.Resolve([]string{"agent", "self-replace", "--image", "x"})
	assert.True(t, ok)
	assert.Equal(t, []string{"agent", "self-replace"}, path)
	assert.Equal(t, map[string]bool{"agent self-replace": true}, cfg.AllowInternal)
	for _, argv := range [][]string{{"up", "web"}, {"delete", "web"}, {"enable", "web"}, {"setup"}, {},
		{"agent"}, {"agent", "replace-helper"}, {"upgrade"}} {
		_, ok := cfg.Resolve(argv)
		assert.False(t, ok, "a worker refuses %v", argv)
	}
	_, ok = WorkerResolver(nil)([]string{"exec", "x"})
	assert.False(t, ok, "no resolver: refuse everything")
}

// A Docker engine outside any swarm is waited on, with a message saying what
// to do, every 10 s — the container must not crash-loop at boot.
func TestAgent_InactiveSwarmWaits(t *testing.T) {
	h := newHarness(t, RoleLeader)
	n := 0
	h.a.Inspect = func(context.Context) (NodeInfo, error) {
		n++
		switch n {
		case 1:
			return NodeInfo{}, errors.New("Cannot connect to the Docker daemon")
		case 2, 3:
			return NodeInfo{Role: RoleNone}, nil
		}
		return NodeInfo{Role: RoleLeader}, nil
	}
	_, stop := h.start()
	require.NoError(t, stop())
	calls := h.rec.Calls()
	require.GreaterOrEqual(t, len(calls), 3)
	assert.Equal(t, []string{"sleep 1s", "sleep 10s", "sleep 10s", "network"}, calls[:4])
	assert.Contains(t, h.rec.Logs(), "waiting for Docker")
	assert.Contains(t, h.rec.Logs(), "run `homelab init` (first PC) or `homelab join`")
}

// The vault is retried with backoff too.
func TestAgent_WaitsForTheVault(t *testing.T) {
	h := newHarness(t, RoleWorker)
	n := 0
	h.a.OpenSecrets = func() (*secrets.Manager, error) {
		n++
		if n < 3 {
			return nil, syscall.EAGAIN
		}
		return h.sm, nil
	}
	_, stop := h.start()
	require.NoError(t, stop())
	assert.Equal(t, []string{"sleep 1s", "sleep 2s"}, h.rec.Calls()[:2])
}

// Cancelled while waiting for the swarm: Run returns cleanly.
func TestAgent_CancelWhileWaiting(t *testing.T) {
	h := newHarness(t, RoleNone)
	ctx, cancel := context.WithCancel(context.Background())
	h.a.Sleep = func(context.Context, time.Duration) bool { cancel(); return false }
	require.NoError(t, h.a.Run(ctx))
	assert.Empty(t, h.rec.Calls())
}

func TestAgent_ShimVolumeHoldsBinary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hl")
	require.NoError(t, os.Mkdir(dir, 0o755)) //nolint:gosec // the mounted volume
	src := filepath.Join(t.TempDir(), "homelab")
	require.NoError(t, os.WriteFile(src, []byte("v1"), 0o700))
	dst := filepath.Join(dir, ShimName)

	require.NoError(t, InstallShim(src, dir))
	got, err := os.ReadFile(dst) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got))
	fi1, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o555), fi1.Mode().Perm())

	// Same content: left alone (no rewrite under running containers).
	require.NoError(t, InstallShim(src, dir))
	fi2, err := os.Stat(dst)
	require.NoError(t, err)
	assert.True(t, os.SameFile(fi1, fi2), "unchanged binary is not rewritten")

	// New content: a new file renamed into place. A container that opened
	// the old binary keeps reading it whole.
	old, err := os.Open(dst) //nolint:gosec // test path
	require.NoError(t, err)
	defer func() { _ = old.Close() }()
	require.NoError(t, os.WriteFile(src, []byte("v2-longer"), 0o700))
	require.NoError(t, InstallShim(src, dir))
	fi3, err := os.Stat(dst)
	require.NoError(t, err)
	assert.False(t, os.SameFile(fi1, fi3), "replaced by rename, not rewritten in place")
	assert.Equal(t, os.FileMode(0o555), fi3.Mode().Perm())
	got, err = os.ReadFile(dst) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, "v2-longer", string(got))
	oldContent := make([]byte, 16)
	n, _ := old.Read(oldContent)
	assert.Equal(t, "v1", string(oldContent[:n]), "the old inode is intact")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp files left behind")
}

// A missing homelab-shim mount is an error naming it — never a shim quietly
// written into the container's own layer.
func TestInstallShim_RequiresTheMountedVolume(t *testing.T) {
	src := filepath.Join(t.TempDir(), "homelab")
	require.NoError(t, os.WriteFile(src, []byte("v1"), 0o700))
	dir := filepath.Join(t.TempDir(), "hl")
	err := InstallShim(src, dir)
	require.ErrorContains(t, err, "homelab-shim volume is not mounted at "+dir)
	assert.NoDirExists(t, dir)
}

// A broken routing file at boot leaves the edge up on an admin-only config:
// the leader logs it loudly and carries on, so DNS, cloudflared, services and
// the remote CLI that can fix it all come up.
func TestAgent_BrokenEdgeConfigDoesNotStopStartup(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeConfig(map[string]string{"DOMAIN": "example.test"})
	require.NoError(t, h.sm.Set("", "CLOUDFLARE_API_TOKEN", "cftok"))
	h.writeDesired(map[string]desired.State{"postgres": desired.Running, "alpha": desired.Running})
	h.edge.startErrs = []error{fmt.Errorf("%w: %w: Caddyfile:40: unrecognized directive: no_such_directive",
		edge.ErrConfigNotLoaded, edge.ErrInvalidConfig)}

	_, stop := h.start()
	require.NoError(t, stop())
	calls := h.rec.Calls()
	assert.Equal(t, 1, countOf(calls, "edge-start-failed"), "not retried: the edge is up")
	assert.Subset(t, calls, []string{"dns cftok example.test *.home.example.test 100.101.102.103",
		"remove-cloudflared", "up postgres", "up alpha", "rebalance", "serve", "edge-stop"})
	assert.Contains(t, h.rec.Logs(), "ERROR: the edge is up but serves no sites")
	assert.Contains(t, h.rec.Logs(), "no_such_directive", "the cause is named")
	assert.NotContains(t, h.rec.Logs(), "cftok", "never a secret")
}

// The tailnet node or the admin socket failing is retried in-process, not by
// crashing the container.
func TestAgent_EdgeStartFailureIsRetried(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.edge.startErrs = []error{errors.New("starting tailnet node: context deadline exceeded"),
		errors.New("checking admin socket: permission denied")}
	_, stop := h.start()
	require.NoError(t, stop())
	calls := h.rec.Calls()
	assert.Equal(t, []string{"network", "edge-start-failed", "sleep 1s", "edge-start-failed", "sleep 2s", "shim", "edge"}, calls[:7])
	assert.Contains(t, calls, "serve")
}

// Another agent on the same state is the one case that is fatal.
func TestAgent_AnotherAgentIsFatal(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeDesired(map[string]desired.State{"alpha": desired.Running})
	h.edge.startErrs = []error{fmt.Errorf("%w: its Caddy admin API answers on /x", edge.ErrAnotherAgent)}
	err := h.a.Run(context.Background())
	require.ErrorIs(t, err, edge.ErrAnotherAgent)
	assert.NotContains(t, h.rec.Calls(), "up alpha")
	assert.NotContains(t, h.rec.Calls(), "serve")
}

// A worker takes TS_AUTHKEY from its vault only (join seeds it there): an
// env var would be visible to anyone who can `docker inspect` the agent.
func TestAgent_WorkerIgnoresTSAuthKeyEnv(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "tskey-env")
	h := newHarness(t, RoleWorker)
	var got string
	h.a.NewNode = func(_ context.Context, hostname, _, authKey string) (Node, error) {
		got = authKey
		return &fakeNode{rec: h.rec, hostname: hostname}, nil
	}
	_, stop := h.start()
	require.NoError(t, stop())
	assert.Empty(t, got)
	assert.NotContains(t, h.rec.Logs(), "tskey-env")
}

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

// The leader copies its own resolved executable, not os.Args[0].
func TestAgent_DefaultBinaryIsResolvedExecutable(t *testing.T) {
	a := &Agent{}
	got, err := a.binary()
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(exe)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.True(t, filepath.IsAbs(got))
}
