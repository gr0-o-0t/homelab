package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/clistate"
	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/diagnostics"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

// stateCluster: minis and a laptop worker. web runs app on minis (healthy)
// and worker on the laptop; busy has one running and one pending task;
// idle is installed but has no Swarm services.
func stateCluster(t *testing.T) (string, *swarmtest.Fake) {
	t.Helper()
	root := t.TempDir()
	for _, n := range []string{"web", "busy", "idle"} {
		writeSvc(t, root, n, map[string]string{"docker-compose.yml": "services: {}\n"})
	}
	f := fakeSwarm(t)
	f.SetNodes([]dswarm.Node{
		{ID: "m1", Description: dswarm.NodeDescription{Hostname: "minis"}},
		{ID: "n2", Description: dswarm.NodeDescription{Hostname: "laptop"}},
	})
	now := time.Now()
	task := func(svc, key, node string, state dswarm.TaskState, desired dswarm.TaskState, cid string) {
		resp, err := f.ServiceCreate(context.Background(), dswarm.ServiceSpec{
			Annotations:  dswarm.Annotations{Name: svc + "_" + key, Labels: map[string]string{swarm.LabelService: svc}},
			TaskTemplate: dswarm.TaskSpec{ContainerSpec: &dswarm.ContainerSpec{Image: "nginx:1@sha256:abc"}},
		}, dswarm.ServiceCreateOptions{})
		require.NoError(t, err)
		st := dswarm.TaskStatus{State: state, Timestamp: now}
		if cid != "" {
			st.ContainerStatus = &dswarm.ContainerStatus{ContainerID: cid}
		}
		f.AddTask(dswarm.Task{ID: "t-" + svc + key, ServiceID: resp.ID, NodeID: node, DesiredState: desired, Status: st})
	}
	task("web", "app", "m1", dswarm.TaskStateRunning, dswarm.TaskStateRunning, "c-app")
	task("web", "worker", "n2", dswarm.TaskStateRunning, dswarm.TaskStateRunning, "c-worker")
	task("busy", "a", "m1", dswarm.TaskStateRunning, dswarm.TaskStateRunning, "c-a")
	task("busy", "b", "m1", dswarm.TaskStatePending, dswarm.TaskStateRunning, "")
	// An old task being shut down is not counted.
	task("busy", "old", "m1", dswarm.TaskStateShutdown, dswarm.TaskStateShutdown, "c-old")
	f.SetContainers([]container.Summary{{ID: "c-app", State: "running", Status: "Up 2 minutes (healthy)"}})
	return root, f
}

func byName(svcs []service.Service) map[string]service.Service {
	m := map[string]service.Service{}
	for _, s := range svcs {
		m[s.Name] = s
	}
	return m
}

// Running and total come from the tasks Swarm wants running, cluster-wide.
func TestDiscover_CountsSwarmTasks(t *testing.T) {
	root, _ := stateCluster(t)
	svcs, err := discoverServices(root)
	require.NoError(t, err)
	m := byName(svcs)

	assert.Equal(t, 2, m["web"].Running)
	assert.Equal(t, 2, m["web"].Total)
	assert.Equal(t, 1, m["busy"].Running)
	assert.Equal(t, 2, m["busy"].Total, "the shut-down task is not counted")
	assert.Zero(t, m["idle"].Total, "no Swarm services: stopped")

	nodes := map[string]string{}
	for _, c := range m["web"].Containers {
		nodes[c.Service] = c.Node
		assert.Equal(t, "nginx:1", c.Image, "the pinned digest is not shown")
	}
	assert.Equal(t, map[string]string{"app": "minis", "worker": "laptop"}, nodes)
	assert.Equal(t, "healthy", service.AggregateHealth(m["web"].Containers))
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	fn()
	require.NoError(t, w.Close())
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// `status <svc>` shows each task with the node it runs on.
func TestStatusService_ShowsNodes(t *testing.T) {
	root, _ := stateCluster(t)
	out := captureStdout(t, func() { require.NoError(t, runServiceStatus(root, "web", map[string]string{})) })
	assert.Contains(t, out, "2/2 running")
	assert.Contains(t, out, "NODE")
	assert.Contains(t, out, "laptop")
	assert.Contains(t, out, "minis")
}

// doctor <svc>: every desired task running passes, with the newest one's
// image and health; a pending task or no tasks at all fails.
func TestDoctor_ServiceTasks(t *testing.T) {
	root, f := stateCluster(t)
	src := &swarm.Deployer{API: f, Root: root}

	g := diagnostics.RunServiceContainerChecks("web", src, false)
	require.Len(t, g.Results, 2)
	assert.Equal(t, diagnostics.StatusPass, g.Results[0].Status)
	assert.Contains(t, g.Results[0].Message, "2/2 tasks running")
	assert.Contains(t, g.Results[1].Message, "nginx:1")

	g = diagnostics.RunServiceContainerChecks("busy", src, false)
	assert.Equal(t, diagnostics.StatusFail, g.Results[0].Status)
	assert.Contains(t, g.Results[0].Message, "1/2 tasks running")
	assert.Contains(t, g.Results[0].Message, "pending")

	g = diagnostics.RunServiceContainerChecks("idle", src, false)
	assert.Equal(t, diagnostics.StatusFail, g.Results[0].Status)
	assert.Contains(t, g.Results[0].Message, "no tasks")

	assert.True(t, runServiceDoctorForQuiet(t, root, "web"))
	assert.False(t, runServiceDoctorForQuiet(t, root, "idle"))

	// A service desired stopped is expected to have no tasks: a warning.
	g = diagnostics.RunServiceContainerChecks("idle", src, true)
	assert.Equal(t, diagnostics.StatusWarn, g.Results[0].Status)
	assert.Contains(t, g.Results[0].Message, "is stopped")
	recordService(root, "idle", desired.Stopped)
	assert.True(t, runServiceDoctorForQuiet(t, root, "idle"))
	// ...and runServiceDoctorWarns counts it, for the `doctor --all` footer.
	var ok bool
	var warns int
	captureStdout(t, func() { ok, warns = runServiceDoctorWarns(root, "idle", false) })
	assert.True(t, ok)
	assert.GreaterOrEqual(t, warns, 1)
}

func runServiceDoctorForQuiet(t *testing.T, root, name string) (ok bool) {
	t.Helper()
	captureStdout(t, func() { ok = runServiceDoctorFor(root, name, false) })
	return ok
}

// With Swarm unreachable, states are unknown rather than "stopped".
func TestDiscover_SwarmUnavailableIsUnknown(t *testing.T) {
	root, f := stateCluster(t)
	f.SetNodes(nil)
	old := newSwarmAPI
	newSwarmAPI = func() (swarm.API, error) { return brokenAPI{f}, nil }
	t.Cleanup(func() { newSwarmAPI = old })

	svcs, err := discoverServices(root)
	require.NoError(t, err)
	for _, s := range svcs {
		assert.True(t, s.StateUnknown, s.Name)
	}
	out := captureStdout(t, func() { printServiceTable(svcs, map[string]string{}, false) })
	assert.Contains(t, out, "unknown")
	assert.NotContains(t, out, "stopped")
}

// brokenAPI is a Swarm API whose every service listing fails, as when the
// daemon is down.
type brokenAPI struct{ *swarmtest.Fake }

func (brokenAPI) ServiceList(context.Context, dswarm.ServiceListOptions) ([]dswarm.Service, error) {
	return nil, errors.New("Cannot connect to the Docker daemon")
}

// `status --json` is what the front ends read: services with counts and
// tasks, the nodes, the edge; `status <svc> --json` is one service.
func TestStatusJSON_ForFrontEnds(t *testing.T) {
	root, _ := stateCluster(t)
	t.Setenv("HOMELAB_CADDY_ADMIN", filepath.Join(t.TempDir(), "none.sock"))
	t.Cleanup(resetFlags)

	out := captureStdout(t, func() { runCLI(t, root, "--json", "status") })
	var st clistate.Status
	require.NoError(t, json.Unmarshal([]byte(out), &st), out)
	assert.False(t, st.Edge.Reachable)
	assert.NotEmpty(t, st.Edge.Error)
	var hosts []string
	for _, n := range st.Nodes {
		hosts = append(hosts, n.Hostname)
	}
	assert.ElementsMatch(t, []string{"minis", "laptop"}, hosts)
	byName := map[string]clistate.Service{}
	for _, s := range st.Services {
		byName[s.Name] = s
	}
	assert.Equal(t, 2, byName["web"].Running)
	assert.Equal(t, 1, byName["busy"].Running)
	assert.Equal(t, 2, byName["busy"].Total)

	out = captureStdout(t, func() { runCLI(t, root, "--json", "status", "web") })
	var web clistate.Service
	require.NoError(t, json.Unmarshal([]byte(out), &web), out)
	require.Len(t, web.Containers, 2)
	nodes := []string{web.Containers[0].Node, web.Containers[1].Node}
	assert.ElementsMatch(t, []string{"minis", "laptop"}, nodes)
}
