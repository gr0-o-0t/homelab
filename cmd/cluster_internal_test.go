package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// On the leader, exec/logs of a service whose task runs on another node go
// to that node's agent, with the argv the leader got (minus its own
// --config-dir).
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
	code = executeArgs([]string{"--config-dir=" + root, "logs", "-n", "5", "alpha"})
	assert.Equal(t, 7, code)
	require.Len(t, gotArgv, 2)
	assert.Equal(t, []string{"exec", "-u", "root", "alpha", "ls", "-la"}, gotArgv[0])
	assert.Equal(t, []string{"logs", "-n", "5", "alpha"}, gotArgv[1])
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

func runningTask(id, svcName string, created int64) container.Summary {
	return container.Summary{ID: id, State: "running", Created: created,
		Labels: map[string]string{"com.docker.swarm.service.name": svcName}}
}

// A task on the leader itself runs here, by container ID, with today's flags.
func TestExec_LocalTaskNotForwarded(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	taskOn(t, f, "alpha", "mini1")
	f.SetContainers([]container.Summary{runningTask("c-old", "alpha_web", 1), runningTask("c-new", "alpha_web", 2)})
	got := stubDocker(t)

	resetFlags()
	require.Zero(t, executeArgs([]string{"--config-dir", root, "exec", "-u", "root", "-w", "/srv", "-e", "A=1", "alpha", "ls", "-la"}))
	resetFlags()
	require.Zero(t, executeArgs([]string{"--config-dir", root, "logs", "-f", "-n", "5", "--since", "1h", "alpha"}))
	require.Len(t, *got, 2)
	assert.Equal(t, []string{"exec", "-i", "--user", "root", "--workdir", "/srv", "--env", "A=1", "c-new", "ls", "-la"}, (*got)[0])
	assert.Equal(t, []string{"logs", "-f", "--tail", "5", "--since", "1h", "c-new"}, (*got)[1])
}

// A worker agent has no service files: it finds the container from its
// labels alone.
func TestExec_WorkerRunsWithoutStateDir(t *testing.T) {
	empty := t.TempDir()
	f := fakeSwarm(t)
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "n2"}})
	f.SetContainers([]container.Summary{runningTask("c1", "alpha_alpha", 1), runningTask("c2", "alpha_db", 1)})
	got := stubDocker(t)

	resetFlags()
	require.Zero(t, executeArgs([]string{"--config-dir", empty, "exec", "-T", "alpha", "sh"}))
	resetFlags()
	require.Zero(t, executeArgs([]string{"--config-dir", empty, "logs", "-t", "alpha"}))
	require.Len(t, *got, 2)
	assert.Equal(t, []string{"exec", "-i", "c1", "sh"}, (*got)[0])
	assert.Equal(t, []string{"logs", "-t", "c1"}, (*got)[1])
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
		forwardableArgv([]string{"--config-dir", "/x", "--no-color", "--config=/y", "--no-record", "exec", "a", "sh"}))
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
