package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

// The hidden cluster commands act on the swarm's control plane: only a
// manager (the leader) runs them.
func TestCluster_RefusedOffManager(t *testing.T) {
	f := fakeSwarm(t)
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "w1"}})
	f.SetNodes([]dswarm.Node{{ID: "w1"}})
	for _, argv := range [][]string{{"cluster", "join-token"}, {"cluster", "label", "w1", "worker"}} {
		resetFlags()
		code := executeArgs(append([]string{"--config-dir", t.TempDir()}, argv...))
		assert.NotZero(t, code, "%v", argv)
	}
	assert.Empty(t, f.Nodes()[0].Spec.Labels)
	assert.True(t, remote.Mutating["cluster"], "cluster takes the server-wide lock")
}

// On the leader, exec of a service whose task runs on another node goes to
// that node's agent, with the argv the leader got (minus its own
// --config-dir), -c included.
func TestExec_ForwardsToTaskNode(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	taskOn(t, f, "alpha", "laptop")

	var gotEndpoint, gotSecret string
	var gotArgv [][]string
	oldRun, oldCluster := relayRun, relayCluster
	relayRun = func(c *remote.Client, argv []string) (int, error) {
		gotEndpoint, gotSecret = c.Endpoint, c.Secret
		gotArgv = append(gotArgv, argv)
		return 7, nil
	}
	relayCluster = func() (agentstate.Cluster, error) {
		return agentstate.Cluster{Secret: "s3cret", Tailnet: "tail1234.ts.net"}, nil
	}
	t.Cleanup(func() { relayRun, relayCluster = oldRun, oldCluster })

	resetFlags()
	code := executeArgs([]string{"--config-dir", root, "exec", "-u", "root", "alpha", "ls", "-la"})
	assert.Equal(t, 7, code, "the remote exit code is ours")
	assert.Equal(t, "https://homelab-laptop.tail1234.ts.net:7443", gotEndpoint)
	assert.Equal(t, "s3cret", gotSecret)

	resetFlags()
	code = executeArgs([]string{"--config-dir=" + root, "exec", "-c", "web", "alpha", "sh"})
	assert.Equal(t, 7, code)
	require.Len(t, gotArgv, 2)
	assert.Equal(t, []string{"exec", "-u", "root", "alpha", "ls", "-la"}, gotArgv[0])
	assert.Equal(t, []string{"exec", "-c", "web", "alpha", "sh"}, gotArgv[1])
}

// stubDocker records the docker CLI invocations instead of running them, and
// fails any relay.
func stubDocker(t *testing.T) *[][]string {
	t.Helper()
	var got [][]string
	oldCLI, oldRun := dockerCLI, relayRun
	dockerCLI = func(args []string) error { got = append(got, args); return nil }
	relayRun = func(*remote.Client, []string) (int, error) { t.Error("forwarded"); return 1, nil }
	t.Cleanup(func() { dockerCLI, relayRun = oldCLI, oldRun })
	return &got
}

// runningTask is a running Swarm task container of stack, labelled as
// translate labels it.
func runningTask(id, stack, key, containerName string, created int64) container.Summary {
	l := map[string]string{"com.docker.swarm.service.name": stack + "_" + key, "com.docker.stack.namespace": stack}
	if containerName != "" {
		l["homelab.container_name"] = containerName
	}
	return container.Summary{ID: id, State: "running", Created: created, Labels: l}
}

// A task on the leader itself runs here, by container ID, with today's flags.
func TestExec_LocalTaskNotForwarded(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	taskOn(t, f, "alpha", "mini1")
	f.SetContainers([]container.Summary{runningTask("c-old", "alpha", "web", "", 1), runningTask("c-new", "alpha", "web", "", 2)})
	got := stubDocker(t)

	resetFlags()
	require.Zero(t, executeArgs([]string{"--config-dir", root, "exec", "-u", "root", "-w", "/srv", "-e", "A=1", "alpha", "ls", "-la"}))
	require.Len(t, *got, 1)
	assert.Equal(t, []string{"exec", "-i", "--user", "root", "--workdir", "/srv", "--env", "A=1", "c-new", "ls", "-la"}, (*got)[0])
}

// A worker agent has no service files: it finds the container from its
// labels alone.
func TestExec_WorkerRunsWithoutStateDir(t *testing.T) {
	empty := t.TempDir()
	f := fakeSwarm(t)
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "n2"}})
	f.SetContainers([]container.Summary{runningTask("c1", "alpha", "alpha", "", 1), runningTask("c2", "alpha", "db", "", 1)})
	got := stubDocker(t)

	resetFlags()
	require.Zero(t, executeArgs([]string{"--config-dir", empty, "exec", "-T", "alpha", "sh"}))
	resetFlags()
	require.Zero(t, executeArgs([]string{"--config-dir", empty, "exec", "-c", "db", "alpha", "psql"}))
	require.Equal(t, [][]string{{"exec", "-i", "c1", "sh"}, {"exec", "-i", "c2", "psql"}}, *got)
	assert.NotContains(t, f.CallLog(), "TaskList", "a worker cannot list tasks")

	resetFlags()
	assert.NotZero(t, executeArgs([]string{"--config-dir", empty, "exec", "beta", "sh"}), "no container of beta here")
}

// The leader still refuses a service it does not have.
func TestExec_LeaderValidatesService(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	taskOn(t, f, "alpha", "mini1")
	got := stubDocker(t)
	resetFlags()
	assert.NotZero(t, executeArgs([]string{"--config-dir", root, "exec", "nosuch", "sh"}))
	assert.Empty(t, *got)
}

// immichStack installs immich in root and deploys its two services, both
// running on the leader: no immich_immich; the server's container_name is
// the stack's name.
func immichStack(t *testing.T, root string, f *swarmtest.Fake) {
	t.Helper()
	dir := filepath.Join(root, "services", "immich")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "m1", ControlAvailable: true}})
	f.SetNodes([]dswarm.Node{{ID: "m1", Description: dswarm.NodeDescription{Hostname: "mini1"}}})
	for _, s := range []struct{ key, cname string }{{"immich-server", "immich"}, {"immich-machine-learning", "immich-machine-learning"}} {
		resp, err := f.ServiceCreate(context.Background(), dswarm.ServiceSpec{Annotations: dswarm.Annotations{
			Name: "immich_" + s.key, Labels: map[string]string{"homelab.service": "immich", "homelab.container_name": s.cname}}},
			dswarm.ServiceCreateOptions{})
		require.NoError(t, err)
		f.AddTask(dswarm.Task{ID: "t-" + s.key, ServiceID: resp.ID, NodeID: "m1", DesiredState: dswarm.TaskStateRunning,
			Status: dswarm.TaskStatus{State: dswarm.TaskStateRunning}})
	}
	f.SetContainers([]container.Summary{
		runningTask("srv", "immich", "immich-server", "immich", 1),
		runningTask("ml", "immich", "immich-machine-learning", "immich-machine-learning", 1),
	})
}

// `exec immich` reaches immich-server (container_name immich); -c takes a
// compose key or a container_name.
func TestExec_ImmichDefaultAndContainerFlag(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	immichStack(t, root, f)
	got := stubDocker(t)
	for _, argv := range [][]string{
		{"exec", "immich", "sh"},
		{"exec", "-c", "immich-machine-learning", "immich", "sh"},
		{"exec", "--container", "immich", "immich", "sh"},
		{"exec", "-c", "immich-server", "immich", "sh"},
	} {
		resetFlags()
		require.Zero(t, executeArgs(append([]string{"--config-dir", root}, argv...)), "%v", argv)
	}
	assert.Equal(t, [][]string{{"exec", "-i", "srv", "sh"}, {"exec", "-i", "ml", "sh"}, {"exec", "-i", "srv", "sh"}, {"exec", "-i", "srv", "sh"}}, *got)

	resetFlags()
	assert.NotZero(t, executeArgs([]string{"--config-dir", root, "exec", "-c", "redis", "immich", "sh"}))
	assert.Len(t, *got, 4)
}

// stubServiceLogs makes every `docker service logs` print two lines naming
// its service, and records the argv.
func stubServiceLogs(t *testing.T) *[][]string {
	t.Helper()
	var mu sync.Mutex
	var got [][]string
	old := serviceLogsCLI
	serviceLogsCLI = func(_ context.Context, args []string, stdout, stderr io.Writer) error {
		mu.Lock()
		got = append(got, args)
		mu.Unlock()
		name := args[len(args)-1]
		_, _ = io.WriteString(stdout, "hello from "+name+"\nsecond ")
		_, _ = io.WriteString(stdout, "line\n")
		_, _ = io.WriteString(stderr, "warn "+name)
		return nil
	}
	t.Cleanup(func() { serviceLogsCLI = old })
	return &got
}

func runLogs(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errb)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })
	resetFlags()
	return executeArgs(argv), out.String(), errb.String()
}

// `logs immich` streams every service of the stack, each line prefixed with
// its compose key, as compose did.
func TestLogs_StreamsEveryServiceWithPrefixes(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	immichStack(t, root, f)
	got := stubServiceLogs(t)

	code, out, errOut := runLogs(t, "--config-dir", root, "logs", "-f", "-n", "50", "-t", "--since", "1h", "immich")
	require.Zero(t, code)
	assert.ElementsMatch(t, [][]string{
		{"service", "logs", "--raw", "--follow", "--timestamps", "--tail", "50", "--since", "1h", "immich_immich-machine-learning"},
		{"service", "logs", "--raw", "--follow", "--timestamps", "--tail", "50", "--since", "1h", "immich_immich-server"},
	}, *got)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	assert.ElementsMatch(t, []string{
		"immich-machine-learning | hello from immich_immich-machine-learning",
		"immich-machine-learning | second line",
		"immich-server           | hello from immich_immich-server",
		"immich-server           | second line",
	}, lines)
	assert.Contains(t, errOut, "immich-server           | warn immich_immich-server\n")
	assert.Empty(t, f.Execs())
}

func TestLogs_ContainerFlagLimitsToOneService(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	immichStack(t, root, f)
	got := stubServiceLogs(t)

	code, out, _ := runLogs(t, "--config-dir", root, "logs", "-c", "immich", "immich")
	require.Zero(t, code)
	require.Len(t, *got, 1)
	assert.Equal(t, "immich_immich-server", (*got)[0][len((*got)[0])-1])
	assert.NotContains(t, out, "machine-learning")
}

func TestLogs_UntilRefused(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	immichStack(t, root, f)
	got := stubServiceLogs(t)
	code, _, _ := runLogs(t, "--config-dir", root, "logs", "--until", "5m", "immich")
	assert.NotZero(t, code)
	assert.Empty(t, *got)
}

// An ambiguous stack is refused for exec with the choices listed.
func TestExec_AmbiguousStackRefused(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "n2"}})
	f.SetContainers([]container.Summary{runningTask("a", "alpha", "web", "alpha-web", 1), runningTask("b", "alpha", "db", "", 1)})
	got := stubDocker(t)
	_ = root
	empty := t.TempDir()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	oldErr := os.Stderr
	os.Stderr = w
	resetFlags()
	code := executeArgs([]string{"--config-dir", empty, "exec", "alpha", "sh"})
	os.Stderr = oldErr
	_ = w.Close()
	msg, _ := io.ReadAll(r)
	assert.NotZero(t, code)
	assert.Contains(t, string(msg), "db, web (alpha-web)")
	assert.Empty(t, *got)
}

func taskOn(t *testing.T, f *swarmtest.Fake, svc, host string) {
	t.Helper()
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "m1", ControlAvailable: true}})
	f.SetNodes([]dswarm.Node{
		{ID: "m1", Description: dswarm.NodeDescription{Hostname: "mini1"}},
		{ID: "n2", Description: dswarm.NodeDescription{Hostname: "laptop"}},
	})
	resp, err := f.ServiceCreate(context.Background(), dswarm.ServiceSpec{Annotations: dswarm.Annotations{
		Name: svc + "_web", Labels: map[string]string{"homelab.service": svc}}}, dswarm.ServiceCreateOptions{})
	require.NoError(t, err)
	node := "m1"
	if host == "laptop" {
		node = "n2"
	}
	f.AddTask(dswarm.Task{ID: "t-" + svc, ServiceID: resp.ID, NodeID: node, DesiredState: dswarm.TaskStateRunning,
		Status: dswarm.TaskStatus{State: dswarm.TaskStateRunning}})
}

func TestForwardableArgv_StripsLeaderFlags(t *testing.T) {
	assert.Equal(t, []string{"--no-color", "exec", "a", "sh"},
		forwardableArgv([]string{"--config-dir", "/x", "--no-color", "--config=/y", "exec", "a", "sh"}))
	// after the service, flags belong to the command
	assert.Equal(t, []string{"exec", "a", "grep", "--config", "z"},
		forwardableArgv([]string{"--config-dir=/x", "exec", "a", "grep", "--config", "z"}))
}

func TestLogin_WritesClientAndKeyring(t *testing.T) {
	fakeSwarm(t) // in-memory keyring via openSecrets
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	sm, err := openSecrets()
	require.NoError(t, err)

	require.NoError(t, login(strings.NewReader("s3cret\n"), true, "https://homelab.tail1234.ts.net:7443"))
	b, err := os.ReadFile(filepath.Join(cfg, "homelab", "client.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(b), "https://homelab.tail1234.ts.net:7443")
	assert.NotContains(t, string(b), "s3cret")
	s, _ := sm.Get("", "CLUSTER_SECRET")
	assert.Equal(t, "s3cret", s)

	require.NoError(t, logout())
	_, err = os.Stat(filepath.Join(cfg, "homelab", "client.yaml"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	s, _ = sm.Get("", "CLUSTER_SECRET")
	assert.Empty(t, s)

	require.Error(t, login(strings.NewReader("s\n"), true, "http://plain:7443"), "https only")
}

func TestLocalOnly_AgentSeedNeverForwardedNorRemote(t *testing.T) {
	assert.True(t, remote.LocalOnly["agent-seed"])
	assert.True(t, remote.Internal["agent-seed"])
}

// join refuses a plain-http endpoint before asking for the secret, and a
// development build refuses to guess an agent image.
func TestJoinInit_RefuseBeforeTouchingAnything(t *testing.T) {
	f := fakeSwarm(t)
	oldRun := hostRun
	hostRun = func(context.Context, string, ...string) ([]byte, error) {
		t.Error("tailscale queried")
		return nil, nil
	}
	t.Cleanup(func() { hostRun = oldRun })
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, _ = w.WriteString("s3cret\n")
	_ = w.Close()
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	resetFlags()
	assert.NotZero(t, executeArgs([]string{"join", "--secret-stdin", "--endpoint", "http://homelab.x.ts.net:7443", "--image", "i:1"}))
	b := make([]byte, 16)
	n, _ := r.Read(b)
	assert.Equal(t, "s3cret\n", string(b[:n]), "the secret was not read")

	resetFlags()
	require.Equal(t, "dev", Version)
	assert.NotZero(t, executeArgs([]string{"init"}), "a dev build needs --image")
	assert.Empty(t, f.CallLog())
}
