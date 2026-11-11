// Package agent is the long-running `homelab agent`: one container per PC,
// started by Docker at boot with nobody logged in.
//
// On the Swarm manager (the leader) it runs the cluster: the overlay network,
// the edge (Caddy on the agent's own tailnet node), the wildcard DNS record
// pointing at that node, cloudflared, the services recorded as running, the
// rebalancer, and the remote CLI endpoint. On a worker it only serves `exec`
// and `logs` for the containers running there, on a tailnet node of its own.
// Both keep the homelab-shim volume holding their own binary, which Swarm
// containers run as their entrypoint shim.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/edge"
	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
)

// Role is what this node is in the swarm.
type Role int

const (
	// RoleNone: the engine is not in a swarm (yet).
	RoleNone Role = iota
	// RoleLeader: a manager — runs the cluster.
	RoleLeader
	// RoleWorker: serves local exec (and version, agent self-replace) only.
	RoleWorker
)

func (r Role) String() string {
	switch r {
	case RoleLeader:
		return "leader"
	case RoleWorker:
		return "worker"
	default:
		return "none"
	}
}

// NodeInfo is what the agent needs to know about its engine.
type NodeInfo struct {
	Role     Role
	Hostname string // the host's name, for the worker's tailnet node
}

const (
	// DefaultShimDir is where the agent container mounts the homelab-shim
	// volume.
	DefaultShimDir = "/hl"
	// ShimName is the binary's name in the shim volume.
	ShimName = "homelab"
	// RemoteAddr is the tailnet address the remote CLI is served on.
	RemoteAddr = ":7443"
	// LeaderHostname is the leader's tailnet node name.
	LeaderHostname = "homelab"
	// InactivePoll is how often a node outside any swarm re-checks.
	InactivePoll = 10 * time.Second
	maxBackoff   = 30 * time.Second
)

// Edge is the leader's network edge (internal/edge.Edge).
type Edge interface {
	Start(ctx context.Context) error
	Stop() error
	TailnetIPv4() netip.Addr
	ListenTLS(network, addr string) (net.Listener, error)
	// SOCKS is the tailnet node's SOCKS5 proxy address and credential,
	// handed to the remote CLI's children only (exec/logs relay).
	SOCKS() (addr, cred string, err error)
}

// Node is a worker's tailnet node (*tsnet.Server).
type Node interface {
	ListenTLS(network, addr string) (net.Listener, error)
	Close() error
}

// Deployer is the part of swarm.Deployer startup uses.
type Deployer interface {
	EnsureNetwork(ctx context.Context) error
	EnsureCloudflared(ctx context.Context, token string) error
	RemoveCloudflared(ctx context.Context) error
	Up(ctx context.Context, svc string) error
	// Replicas is the replica count of each Swarm service of svc; empty
	// when it has none.
	Replicas(ctx context.Context, svc string) (map[string]uint64, error)
	Scale(ctx context.Context, svc string, replicas map[string]uint64) error
}

// DNSFunc makes name an A record (DNS only, not proxied) to ip in zone,
// changing it only when it differs.
type DNSFunc func(ctx context.Context, token, zone, name string, ip netip.Addr) error

// Agent is one PC's agent. Root is required; every other field has a
// production default or is optional, and tests replace them.
type Agent struct {
	Root    string // the state volume (agentstate.Root)
	ShimDir string // the homelab-shim volume's mount (DefaultShimDir)
	Binary  string // this executable; default os.Executable, resolved
	Version string

	// Resolve maps argv to a command path (cmd.ResolveCommand). Without it
	// the remote endpoint refuses every command.
	Resolve remote.Resolver

	// API is the Docker engine; the default Deployer and Rebalancer use it.
	API swarm.API
	// Inspect reports the engine's swarm role. Required.
	Inspect func(ctx context.Context) (NodeInfo, error)
	// OpenSecrets opens the vault; default secrets.OpenVault on Root's paths.
	OpenSecrets func() (*secrets.Manager, error)
	// NewEdge builds the leader's edge; default *edge.Edge.
	NewEdge func(authKey string, env func() (map[string]string, error)) Edge
	// NewNode brings up a worker's tailnet node; default tsnet.
	NewNode func(ctx context.Context, hostname, dir, authKey string) (Node, error)
	// NewDeployer; default *swarm.Deployer on API.
	NewDeployer func(sm *secrets.Manager) Deployer
	// EnsureDNS keeps the wildcard record on the tailnet IP; nil skips it.
	EnsureDNS DNSFunc
	// Rebalance runs until ctx is done; default swarm.Rebalancer on API.
	Rebalance func(ctx context.Context) error
	// Serve serves the remote CLI on ln until ln is closed; default
	// remote.NewServer(cfg).Serve(ln).
	Serve func(ln net.Listener, cfg remote.ServerConfig) error

	// Sleep waits d or until ctx is done (false); default a timer.
	Sleep func(ctx context.Context, d time.Duration) bool
	// BringUpTimeout bounds each service's deploy at boot; default 10 minutes.
	BringUpTimeout time.Duration
	// Logf logs progress and recovered failures; default log.Printf.
	Logf func(format string, args ...any)
}

func (a *Agent) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (a *Agent) sleep(ctx context.Context, d time.Duration) bool {
	if a.Sleep != nil {
		return a.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func backoff(attempt int) time.Duration {
	d := time.Second << min(attempt, 5)
	return min(d, maxBackoff)
}

// retry runs fn until it succeeds or ctx is done, logging each failure.
func (a *Agent) retry(ctx context.Context, what string, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		d := backoff(attempt)
		a.logf("agent: %s: %v — retrying in %s", what, err, d)
		if !a.sleep(ctx, d) {
			return ctx.Err()
		}
	}
}

// Run brings the agent up for its role and serves until ctx is done
// (SIGTERM). Shutting down stops serving, the rebalancer and the edge; it
// never scales a service down — Swarm keeps them running.
func (a *Agent) Run(ctx context.Context) error {
	if a.Root == "" {
		return errors.New("agent: no state root")
	}
	if a.Inspect == nil {
		return errors.New("agent: no engine inspector")
	}
	paths := agentstate.At(a.Root)

	var sm *secrets.Manager
	if err := a.retry(ctx, "opening the vault", func() error {
		var err error
		sm, err = a.openSecrets(paths)
		return err
	}); err != nil {
		return nil // cancelled while waiting
	}
	info, ok := a.waitForSwarm(ctx)
	if !ok {
		return nil
	}
	cluster, err := agentstate.LoadCluster(paths, sm)
	if err != nil {
		a.logf("agent: reading the cluster settings: %v", err)
	}
	if cluster.Secret == "" {
		a.logf("agent: no cluster secret in the vault — the remote CLI refuses every client until `homelab init` or `homelab join` stores one")
	}
	a.logf("agent: this node is a swarm %s", info.Role)
	if info.Role == RoleLeader {
		return a.runLeader(ctx, paths, sm, cluster.Secret)
	}
	return a.runWorker(ctx, paths, sm, cluster.Secret, info.Hostname)
}

func (a *Agent) openSecrets(p agentstate.Paths) (*secrets.Manager, error) {
	if a.OpenSecrets != nil {
		return a.OpenSecrets()
	}
	return secrets.OpenVault(p.VaultFile, p.KeyFile)
}

// waitForSwarm waits for the engine to answer and be in a swarm.
func (a *Agent) waitForSwarm(ctx context.Context) (NodeInfo, bool) {
	for attempt := 0; ; {
		info, err := a.Inspect(ctx)
		var d time.Duration
		switch {
		case err != nil:
			d = backoff(attempt)
			attempt++
			a.logf("agent: waiting for Docker: %v — retrying in %s", err, d)
		case info.Role == RoleNone:
			attempt = 0
			d = InactivePoll
			a.logf("agent: this Docker engine is not in a swarm — run `homelab init` (first PC) or `homelab join` (the others); checking again in %s", d)
		default:
			return info, true
		}
		if !a.sleep(ctx, d) {
			return NodeInfo{}, false
		}
	}
}

func (a *Agent) binary() (string, error) {
	if a.Binary != "" {
		return a.Binary, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func (a *Agent) shimDir() string {
	if a.ShimDir != "" {
		return a.ShimDir
	}
	return DefaultShimDir
}

// shim copies this binary into the shim volume. A failure is logged: the
// containers already running keep the binary they started with.
func (a *Agent) shim() {
	bin, err := a.binary()
	if err == nil {
		err = InstallShim(bin, a.shimDir())
	}
	if err != nil {
		a.logf("agent: updating the shim volume: %v", err)
	}
}

// InstallShim makes dir/homelab a mode-0555 copy of src. It writes only when
// the content differs, through a temp file renamed into place, so a
// container starting from the volume never sees a partial binary. dir must
// already exist: it is the homelab-shim volume's mount, and creating it would
// put the shim in the container's own layer, where no service sees it.
func InstallShim(src, dir string) error {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("the homelab-shim volume is not mounted at %s (the agent container needs -v homelab-shim:%s)", dir, dir)
	}
	want, err := os.ReadFile(src) //nolint:gosec // our own executable
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, ShimName)
	if have, err := os.ReadFile(dst); err == nil && bytes.Equal(have, want) { //nolint:gosec // path built from the shim dir
		if fi, err := os.Stat(dst); err == nil && fi.Mode().Perm() == 0o555 {
			return nil
		}
	}
	tmp, err := os.CreateTemp(dir, "."+ShimName+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(want); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o555); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	// Make the rename itself durable, or a crash can bring back the old name.
	d, err := os.Open(dir) //nolint:gosec // the shim dir
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// LeaderSecrets are the root secrets the leader reads straight from the
// vault, declared in config.yaml or not.
var LeaderSecrets = []string{"TS_AUTHKEY", "CLOUDFLARE_API_TOKEN", "CF_TUNNEL_TOKEN"}

// leaderEnv is the root config.yaml vars and secrets, plus LeaderSecrets.
func leaderEnv(p agentstate.Paths, sm *secrets.Manager) (map[string]string, error) {
	env, err := config.BuildEnv(config.RootConfigFile(p.StateDir, ""), p.StateDir, "", sm)
	if env == nil {
		env = map[string]string{}
	}
	for _, k := range LeaderSecrets {
		if env[k] != "" {
			continue
		}
		v, gerr := sm.Get("", k)
		if gerr != nil {
			err = errors.Join(err, fmt.Errorf("secret %s: %w", k, gerr))
			continue
		}
		if v != "" {
			env[k] = v
		}
	}
	return env, err
}

// WildcardName is the record every tailnet service name resolves through.
func WildcardName(env map[string]string) string {
	sub := env["HOME_SUBDOMAIN"]
	if sub == "" {
		sub = "home"
	}
	return "*." + sub + "." + env["DOMAIN"]
}

func (a *Agent) runLeader(ctx context.Context, p agentstate.Paths, sm *secrets.Manager, secret string) error {
	env, err := leaderEnv(p, sm)
	if err != nil {
		a.logf("agent: reading config.yaml and the vault: %v", err)
	}
	dep := a.deployer(p, sm)

	// 1. network
	if err := a.retry(ctx, "creating the home-services network", func() error { return dep.EnsureNetwork(ctx) }); err != nil {
		return nil
	}
	// 2. shim
	a.shim()
	// 3. edge
	ed := a.edge(p, sm, env["TS_AUTHKEY"])
	if up, err := a.startEdge(ctx, ed); !up {
		return err // another agent runs here, or cancelled while retrying (nil)
	}
	defer func() {
		if err := ed.Stop(); err != nil {
			a.logf("agent: stopping the edge: %v", err)
		}
	}()
	// 4. DNS
	a.ensureDNS(ctx, env, ed.TailnetIPv4())
	// 5. cloudflared
	if tok := env["CF_TUNNEL_TOKEN"]; tok != "" {
		if err := dep.EnsureCloudflared(ctx, tok); err != nil {
			a.logf("agent: cloudflared: %v", err)
		}
	} else if err := dep.RemoveCloudflared(ctx); err != nil {
		a.logf("agent: removing cloudflared (no CF_TUNNEL_TOKEN): %v", err)
	}
	// 6. serve — before the services come up (R53): the remote CLI answers
	// at once after a boot, and `homelab upgrade`'s check of the new leader
	// does not wait on deploys.
	ln, err := ed.ListenTLS("tcp", RemoteAddr)
	if err != nil {
		return fmt.Errorf("listening on the tailnet %s: %w", RemoteAddr, err)
	}
	// bringUp holds the server's lock one service at a time: a mutating command
	// waits for the service in progress, then runs between services
	// (reads such as status do not take the lock and run meanwhile).
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	cfg := remote.ServerConfig{
		StateDir:   p.StateDir,
		StateRoot:  a.Root,
		Secret:     secret,
		Version:    a.Version,
		Vault:      p.VaultFile,
		VaultKey:   p.KeyFile,
		CaddyAdmin: edge.AdminSocket(p.StateDir),
		Resolve:    a.Resolve,
		Lock:       lock,
	}
	// The relay of exec to workers dials them through the edge's tailnet
	// node; without it only local containers are reachable.
	if addr, cred, err := ed.SOCKS(); err != nil {
		a.logf("agent: warning: no tailnet proxy for relaying exec to other nodes: %v", err)
	} else {
		cfg.TSSocks, cfg.TSSocksCred = addr, cred
	}
	served := make(chan error, 1)
	go func() { served <- a.serve(ctx, ln, cfg) }()

	// 7. services: shared providers first, then the rest
	a.bringUp(ctx, p, dep, lock)
	if ctx.Err() != nil {
		return <-served
	}
	// 8. rebalancer
	// Stopped explicitly once serving has stopped, not by ctx: shutdown is
	// serve → rebalancer → edge.
	rctx, stopRebalance := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := a.rebalance(rctx); err != nil {
			a.logf("agent: rebalancer: %v", err)
		}
	}()
	defer func() { stopRebalance(); wg.Wait() }()
	return <-served
}

// startEdge starts the edge, retrying with backoff while the tailnet node or
// the admin socket fails. A config that does not load leaves the edge up
// (admin only) and is logged loudly: the rest of startup — DNS, cloudflared,
// services, the remote CLI that can repair it — must not hang on a routing
// file. Another agent answering on the admin socket is fatal (err).
func (a *Agent) startEdge(ctx context.Context, ed Edge) (up bool, err error) {
	for attempt := 0; ; attempt++ {
		err := ed.Start(ctx)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, edge.ErrConfigNotLoaded):
			a.logf("agent: ERROR: the edge is up but serves no sites — its config did not load: %v. "+
				"Fix the file it names, then run `homelab reload` (or enable/disable) to load it.", err)
			return true, nil
		case errors.Is(err, edge.ErrAnotherAgent):
			return false, fmt.Errorf("starting the edge: %w", err)
		}
		d := backoff(attempt)
		a.logf("agent: starting the edge: %v — retrying in %s", err, d)
		if !a.sleep(ctx, d) {
			return false, nil
		}
	}
}

func (a *Agent) runWorker(ctx context.Context, p agentstate.Paths, sm *secrets.Manager, secret, hostname string) error {
	a.shim()
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	authKey, err := sm.Get("", "TS_AUTHKEY")
	if err != nil {
		a.logf("agent: reading TS_AUTHKEY: %v", err)
	}
	if authKey == "" {
		// `homelab join` seeds it; never from the environment, which anyone
		// who can `docker inspect` the agent container could read.
		a.logf("agent: no TS_AUTHKEY in the vault — run `homelab join` on this PC")
	}
	newNode := a.NewNode
	if newNode == nil {
		newNode = tsnetNode
	}
	var node Node
	if err := a.retry(ctx, "starting the tailnet node", func() error {
		var err error
		node, err = newNode(ctx, "homelab-"+hostname, filepath.Join(a.Root, "tsnet"), authKey)
		return err
	}); err != nil {
		return nil
	}
	defer func() { _ = node.Close() }()
	ln, err := node.ListenTLS("tcp", RemoteAddr)
	if err != nil {
		return fmt.Errorf("listening on the tailnet %s: %w", RemoteAddr, err)
	}
	cfg := remote.ServerConfig{
		StateDir:  p.StateDir,
		StateRoot: a.Root,
		Secret:    secret,
		Version:   a.Version,
		Vault:     p.VaultFile,
		VaultKey:  p.KeyFile,
		Resolve:   WorkerResolver(a.Resolve),
		// agent is Internal: the server refuses it unless allowed here.
		AllowInternal: map[string]bool{SelfReplacePath: true},
	}
	return a.serve(ctx, ln, cfg)
}

// workerCommands are all a worker agent runs: exec acts on its own
// containers (logs resolves too, but a service's logs refuse off the leader,
// R39: they are read there through docker service logs), version is how
// `homelab upgrade` checks it (and that a replaced agent is a new one).
// Everything else goes to the leader.
var workerCommands = map[string]bool{"exec": true, "logs": true, "version": true}

// SelfReplacePath is the hidden command through which the leader's
// `homelab upgrade` pulls the new image on a worker and replaces the
// worker's agent container. Only worker agents run it for a client.
const SelfReplacePath = "agent self-replace"

// WorkerResolver wraps r so that only `exec`, `logs`, `version` and `agent
// self-replace` resolve; every other command is refused as unknown.
func WorkerResolver(r remote.Resolver) remote.Resolver {
	return func(argv []string) ([]string, bool) {
		if r == nil {
			return nil, false
		}
		path, ok := r(argv)
		if !ok || len(path) == 0 || (!workerCommands[path[0]] && strings.Join(path, " ") != SelfReplacePath) {
			return nil, false
		}
		return path, true
	}
}

// serve runs the remote endpoint on ln until ctx is done, then closes ln.
func (a *Agent) serve(ctx context.Context, ln net.Listener, cfg remote.ServerConfig) error {
	if bin, err := a.binary(); err == nil {
		cfg.Binary = bin
	}
	serve := a.Serve
	if serve == nil {
		serve = func(ln net.Listener, cfg remote.ServerConfig) error { return remote.NewServer(cfg).Serve(ln) }
	}
	done := make(chan error, 1)
	go func() { done <- serve(ln, cfg) }()
	a.logf("agent: serving the remote CLI on the tailnet %s", RemoteAddr)
	select {
	case <-ctx.Done():
		_ = ln.Close()
		<-done
		a.logf("agent: stopped serving")
		return nil
	case err := <-done:
		_ = ln.Close()
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("remote CLI endpoint: %w", err)
	}
}

func (a *Agent) deployer(p agentstate.Paths, sm *secrets.Manager) Deployer {
	if a.NewDeployer != nil {
		return a.NewDeployer(sm)
	}
	return &swarm.Deployer{API: a.API, Root: p.StateDir, SM: sm}
}

func (a *Agent) edge(p agentstate.Paths, sm *secrets.Manager, authKey string) Edge {
	env := func() (map[string]string, error) { return leaderEnv(p, sm) }
	if a.NewEdge != nil {
		return a.NewEdge(authKey, env)
	}
	return &edge.Edge{
		Root:       p.StateDir,
		TSAuthKey:  authKey,
		TSHostname: LeaderHostname,
		TSStateDir: filepath.Join(a.Root, "tsnet"),
		Env:        env,
	}
}

func (a *Agent) rebalance(ctx context.Context) error {
	if a.Rebalance != nil {
		return a.Rebalance(ctx)
	}
	return (&swarm.Rebalancer{API: a.API}).Run(ctx)
}

// ensureDNS points the wildcard record at the tailnet node. Without a
// Cloudflare token or a domain it only warns: the services stay reachable
// on the tailnet by IP.
func (a *Agent) ensureDNS(ctx context.Context, env map[string]string, ip netip.Addr) {
	if a.EnsureDNS == nil {
		return
	}
	token, zone := env["CLOUDFLARE_API_TOKEN"], env["DOMAIN"]
	switch {
	case token == "" || zone == "":
		a.logf("agent: warning: CLOUDFLARE_API_TOKEN or DOMAIN not set — not updating the wildcard DNS record; services are reachable on the tailnet by IP only")
		return
	case !ip.Is4():
		a.logf("agent: warning: the tailnet node has no IPv4 — not updating the wildcard DNS record")
		return
	}
	name := WildcardName(env)
	if err := a.EnsureDNS(ctx, token, zone, name, ip); err != nil {
		a.logf("agent: warning: DNS record %s → %s: %v", name, ip, err)
		return
	}
	a.logf("agent: DNS %s → %s", name, ip)
}

// bringUp deploys every service recorded as running that has no Swarm
// services yet, shared providers first. A service already in Swarm is left
// as Swarm keeps it (R53): redeploying it would roll its tasks and, for a
// floating tag, upgrade it — `homelab update` does that, deliberately. The
// one repair it gets is a container left at zero replicas (a backup cut
// short by the agent stopping) scaled back to one. A service that fails is
// logged and the rest still come up. Each service is deployed under its own
// timeout and its own turn of the lock (see bringUpOne).
func (a *Agent) bringUp(ctx context.Context, p agentstate.Paths, dep Deployer, lock chan struct{}) {
	// The caller hands the lock over held. It is released between services so
	// a waiting mutating command gets a turn, and is not held on return.
	held := lock != nil
	release := func() {
		if held {
			<-lock
			held = false
		}
	}
	defer release()
	d, _, err := desired.LoadDesired(p.StateDir)
	if err != nil {
		a.logf("agent: reading the desired state: %v", err)
		return
	}
	for _, svc := range d.RunningServices() {
		if ctx.Err() != nil {
			return
		}
		a.bringUpOne(ctx, svc, dep)
		if lock != nil {
			release()
			select {
			case lock <- struct{}{}:
				held = true
			case <-ctx.Done():
				return
			}
		}
	}
}

// bringUpOne brings up one service; a deploy (which may pull an image) is
// cut off after BringUpTimeout so a stalled registry cannot hold the lock.
func (a *Agent) bringUpOne(ctx context.Context, svc string, dep Deployer) {
	reps, err := dep.Replicas(ctx, svc)
	if err != nil {
		a.logf("agent: checking %s in Swarm: %v — left as it is", svc, err)
		return
	}
	if len(reps) > 0 {
		zero := map[string]uint64{}
		for name, n := range reps {
			if n == 0 {
				zero[name] = 1
			}
		}
		if len(zero) == 0 {
			return
		}
		if err := dep.Scale(ctx, svc, zero); err != nil {
			a.logf("agent: scaling %s back up: %v", svc, err)
			return
		}
		a.logf("agent: %s was at zero replicas; scaled back up", svc)
		return
	}
	timeout := a.BringUpTimeout
	if timeout <= 0 {
		timeout = defaultBringUpTimeout
	}
	uctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := dep.Up(uctx, svc); err != nil {
		if uctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			a.logf("agent: %s left undeployed: timed out after %s — run `homelab up %s` once the registry is reachable", svc, timeout, svc)
			return
		}
		a.logf("agent: bringing up %s: %v", svc, err)
		return
	}
	a.logf("agent: %s is up", svc)
}

// defaultBringUpTimeout bounds one service's deploy at boot.
const defaultBringUpTimeout = 10 * time.Minute
