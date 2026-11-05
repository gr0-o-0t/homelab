package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// A task on the leader itself is not forwarded.
func TestExec_LocalTaskNotForwarded(t *testing.T) {
	root := makeBatchFixture(t)
	f := fakeSwarm(t)
	taskOn(t, f, "alpha", "mini1")
	oldRun := relayRun
	relayRun = func(*remote.Client, []string) (int, error) { t.Fatal("forwarded"); return 0, nil }
	t.Cleanup(func() { relayRun = oldRun })
	_, local, err := locateTask("alpha")
	require.NoError(t, err)
	assert.True(t, local)
	_ = root
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
