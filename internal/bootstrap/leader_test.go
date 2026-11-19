package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm/swarmtest"
)

func TestLeaderServiceSpec(t *testing.T) {
	s := LeaderServiceSpec("ghcr.io/gr0-o-0t/homelab:v1.2.3")
	assert.Equal(t, AgentService, s.Name)
	assert.Equal(t, "agent", s.Labels["homelab.service"])

	cs := s.TaskTemplate.ContainerSpec
	require.NotNil(t, cs)
	assert.Equal(t, "ghcr.io/gr0-o-0t/homelab:v1.2.3", cs.Image)
	assert.Equal(t, []string{"agent"}, cs.Args)
	assert.Contains(t, cs.Env, "XDG_DATA_HOME=/var/lib/homelab/xdg/data")
	assert.Contains(t, cs.Env, "XDG_CONFIG_HOME=/var/lib/homelab/xdg/config")
	assert.ElementsMatch(t, []mount.Mount{
		{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
		{Type: mount.TypeVolume, Source: "homelab-agent", Target: "/var/lib/homelab"},
		{Type: mount.TypeVolume, Source: "homelab-shim", Target: "/hl"},
		{Type: mount.TypeVolume, Source: "homelab-backups", Target: "/var/lib/homelab-backups"},
	}, cs.Mounts)

	require.Len(t, s.TaskTemplate.Networks, 1)
	assert.Equal(t, "home-services", s.TaskTemplate.Networks[0].Target)
	assert.Equal(t, []string{"homelab-edge"}, s.TaskTemplate.Networks[0].Aliases)

	// Never on a worker, whatever its labels.
	require.NotNil(t, s.TaskTemplate.Placement)
	assert.ElementsMatch(t, []string{"node.role==manager", "node.labels.homelab.role==core"}, s.TaskTemplate.Placement.Constraints)

	require.NotNil(t, s.Mode.Replicated)
	assert.Equal(t, uint64(1), *s.Mode.Replicated.Replicas)
	require.NotNil(t, s.TaskTemplate.RestartPolicy)
	assert.Equal(t, dswarm.RestartPolicyConditionAny, s.TaskTemplate.RestartPolicy.Condition)
	assert.Nil(t, s.TaskTemplate.RestartPolicy.MaxAttempts, "Swarm retries forever")

	// Never two leader agents at once: stop-first both ways, rolled back on failure.
	require.NotNil(t, s.UpdateConfig)
	assert.Equal(t, dswarm.UpdateOrderStopFirst, s.UpdateConfig.Order)
	assert.Equal(t, dswarm.UpdateFailureActionRollback, s.UpdateConfig.FailureAction)
	assert.Equal(t, 30*time.Second, s.UpdateConfig.Monitor)
	require.NotNil(t, s.RollbackConfig)
	assert.Equal(t, dswarm.UpdateOrderStopFirst, s.RollbackConfig.Order)
}

// A worker's agent stays a plain container with no network: its start at
// boot never needs the overlay, so it cannot fail the way the leader's did.
func TestAgentSpec_WorkerHasNoNetwork(t *testing.T) {
	_, _, nc := AgentSpec("img", false)
	assert.Empty(t, nc.EndpointsConfig)
}

func TestAgentContainerID(t *testing.T) {
	ctx := context.Background()

	t.Run("none", func(t *testing.T) {
		_, err := AgentContainerID(ctx, swarmtest.New())
		assert.True(t, errdefs.IsNotFound(err), "%v", err)
	})

	t.Run("plain container (a worker)", func(t *testing.T) {
		f := swarmtest.New()
		require.NoError(t, StartAgent(ctx, f, "img", false))
		id, err := AgentContainerID(ctx, f)
		require.NoError(t, err)
		assert.Equal(t, f.Created()[0].ID, id)
	})

	t.Run("the leader service's task", func(t *testing.T) {
		f := swarmtest.New()
		f.RunServiceTasks()
		_, err := f.ServiceCreate(ctx, LeaderServiceSpec("img"), dswarm.ServiceCreateOptions{})
		require.NoError(t, err)
		id, err := AgentContainerID(ctx, f)
		require.NoError(t, err)
		require.Len(t, f.Created(), 1)
		assert.Equal(t, f.Created()[0].ID, id)
	})
}

var noWait = func(context.Context, time.Duration) bool { return true }

// leaderEngine is the leader's engine running the agent as a plain
// container on oldImage (an older init, the swap of an older upgrade, or
// the manual recovery), with Swarm scheduling service tasks.
func leaderEngine(t *testing.T) *swarmtest.Fake {
	t.Helper()
	f := startedAgent(t, true)
	f.RunServiceTasks()
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, ControlAvailable: true}})
	return f
}

// convert runs the whole conversion on f: what StartConvert starts, then
// what the helper container does.
func convert(t *testing.T, f *swarmtest.Fake) error {
	t.Helper()
	require.NoError(t, StartConvert(context.Background(), f, oldImage, &bytes.Buffer{}))
	spec := helperSpec(t, f)
	require.True(t, spec.ToService)
	return RunReplace(context.Background(), f, spec, noWait)
}

func names(f *swarmtest.Fake) []string {
	var n []string
	for _, c := range f.Created() {
		n = append(n, c.Name)
	}
	return n
}

func TestConvert_PlainLeaderBecomesTheService(t *testing.T) {
	f := leaderEngine(t)
	require.NoError(t, convert(t, f))

	s, _, err := f.ServiceInspectWithRaw(context.Background(), AgentService, dswarm.ServiceInspectOptions{})
	require.NoError(t, err)
	assert.Equal(t, oldImage, s.Spec.TaskTemplate.ContainerSpec.Image, "the image the container ran")
	id, err := AgentContainerID(context.Background(), f)
	require.NoError(t, err)
	assert.Equal(t, created(t, f, names(f)[0]).ID, id)
	assert.NotContains(t, names(f), AgentContainer, "the plain container is gone")
	assert.NotContains(t, names(f), OldContainer)
	assert.NotContains(t, names(f), HelperContainer, "the helper removed itself")

	// Never two leaders: the old agent stops before the service exists.
	log := strings.Join(f.CallLog(), "\n")
	assert.Less(t, strings.Index(log, "ContainerStop"), strings.Index(log, "ServiceCreate "+AgentService))
	for _, c := range f.CallLog() {
		assert.NotContains(t, c, "VolumeRemove")
	}
}

func TestConvert_FailingTaskPutsTheContainerBack(t *testing.T) {
	f := leaderEngine(t)
	f.FailTasks(AgentService, true)
	err := convert(t, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the old agent was put back")

	_, _, ierr := f.ServiceInspectWithRaw(context.Background(), AgentService, dswarm.ServiceInspectOptions{})
	assert.True(t, errdefs.IsNotFound(ierr), "the service it created is removed")
	c := created(t, f, AgentContainer)
	assert.True(t, c.Started, "the old agent runs again under its name")
	assert.Contains(t, names(f), HelperContainer, "the failed helper stays for its logs")
	// The service (and its task) went before the old agent started again.
	log := f.CallLog()
	assert.Less(t, slices.Index(log, "ServiceRemove "+AgentService), lastIndex(log, "ContainerStart"))
}

// A plain leader container next to a running service (the manual recovery
// run although the service was fine): it only takes itself away.
func TestConvert_ServiceAlreadyRunning(t *testing.T) {
	f := leaderEngine(t)
	_, err := f.ServiceCreate(context.Background(), LeaderServiceSpec(newImage), dswarm.ServiceCreateOptions{})
	require.NoError(t, err)
	require.NoError(t, convert(t, f))

	s, _, err := f.ServiceInspectWithRaw(context.Background(), AgentService, dswarm.ServiceInspectOptions{})
	require.NoError(t, err)
	assert.Equal(t, newImage, s.Spec.TaskTemplate.ContainerSpec.Image, "the running service is left as it is")
	assert.NotContains(t, f.CallLog(), "ServiceUpdate "+s.ID)
	assert.NotContains(t, names(f), AgentContainer)
	assert.NotContains(t, names(f), OldContainer)
}

// A service left with no task (scaled to zero) takes the container's image
// and one replica.
func TestConvert_IdleServiceIsReused(t *testing.T) {
	f := leaderEngine(t)
	spec := LeaderServiceSpec(newImage)
	zero := uint64(0)
	spec.Mode.Replicated.Replicas = &zero
	_, err := f.ServiceCreate(context.Background(), spec, dswarm.ServiceCreateOptions{})
	require.NoError(t, err)
	require.NoError(t, convert(t, f))

	s, _, err := f.ServiceInspectWithRaw(context.Background(), AgentService, dswarm.ServiceInspectOptions{})
	require.NoError(t, err)
	assert.Equal(t, oldImage, s.Spec.TaskTemplate.ContainerSpec.Image)
	assert.Equal(t, uint64(1), *s.Spec.Mode.Replicated.Replicas)
	_, err = AgentContainerID(context.Background(), f)
	require.NoError(t, err)
	assert.NotContains(t, names(f), AgentContainer)
}

func TestConvert_RefusedOnAWorker(t *testing.T) {
	f := startedAgent(t, false)
	f.RunServiceTasks()
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive}})
	spec, err := ReplacementSpec(context.Background(), f, AgentContainer, oldImage)
	require.NoError(t, err)
	spec.ToService = true
	require.ErrorContains(t, RunReplace(context.Background(), f, spec, noWait), "not a swarm manager")
	assert.True(t, created(t, f, AgentContainer).Started, "the worker's agent is untouched")
	for _, c := range f.CallLog() {
		assert.False(t, strings.HasPrefix(c, "Service") || strings.HasPrefix(c, "ContainerStop"), "touched: %s", c)
	}
}

func TestPrepareLeader(t *testing.T) {
	ctx := context.Background()

	t.Run("already the service's task", func(t *testing.T) {
		f := swarmtest.New()
		f.RunServiceTasks()
		_, err := f.ServiceCreate(ctx, LeaderServiceSpec(newImage), dswarm.ServiceCreateOptions{})
		require.NoError(t, err)
		self := f.Created()[0].ID[:12]
		handed, err := PrepareLeader(ctx, f, self, noWait, &bytes.Buffer{})
		require.NoError(t, err)
		assert.False(t, handed)
		assert.NotContains(t, names(f), HelperContainer)
	})

	t.Run("a plain container hands over to the helper", func(t *testing.T) {
		f := leaderEngine(t)
		self := created(t, f, AgentContainer).ID[:12]
		handed, err := PrepareLeader(ctx, f, self, noWait, &bytes.Buffer{})
		require.NoError(t, err)
		assert.True(t, handed)
		spec := helperSpec(t, f)
		assert.True(t, spec.ToService)
		assert.Equal(t, oldImage, spec.Config.Image, "converted on the image it runs")
	})

	// The swap of an older upgrade is still watching this container: its
	// helper would roll back a container that stops now. Wait it out.
	t.Run("waits for an older replacement to finish", func(t *testing.T) {
		f := leaderEngine(t)
		self := created(t, f, AgentContainer).ID[:12]
		_, err := f.ContainerCreate(ctx, &container.Config{Image: oldImage, Labels: map[string]string{HelperLabel: "1"}},
			&container.HostConfig{}, nil, nil, HelperContainer)
		require.NoError(t, err)
		require.NoError(t, f.ContainerStart(ctx, HelperContainer, container.StartOptions{}))
		polls := 0
		sleep := func(context.Context, time.Duration) bool {
			polls++
			if polls == 3 { // the old helper finishes and removes itself
				require.NoError(t, f.ContainerRemove(ctx, HelperContainer, container.RemoveOptions{Force: true}))
			}
			return true
		}
		handed, err := PrepareLeader(ctx, f, self, sleep, &bytes.Buffer{})
		require.NoError(t, err)
		assert.True(t, handed)
		assert.GreaterOrEqual(t, polls, 3)
	})

	t.Run("gives up, and keeps running, when it never finishes", func(t *testing.T) {
		f := leaderEngine(t)
		self := created(t, f, AgentContainer).ID[:12]
		_, err := f.ContainerCreate(ctx, &container.Config{Image: oldImage}, &container.HostConfig{}, nil, nil, OldContainer)
		require.NoError(t, err)
		handed, err := PrepareLeader(ctx, f, self, noWait, &bytes.Buffer{})
		require.ErrorContains(t, err, OldContainer)
		assert.False(t, handed)
		assert.NotContains(t, names(f), HelperContainer)
	})
}

func lastIndex(s []string, prefix string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if strings.HasPrefix(s[i], prefix) {
			return i
		}
	}
	return -1
}

// The leader as a service is upgraded by a service update: Swarm stops the
// old task first, watches the new one and rolls back on failure.
func TestUpgrade_LeaderServiceIsUpdated(t *testing.T) {
	f := upgradeFake()
	f.RunServiceTasks()
	_, err := f.ServiceCreate(context.Background(), LeaderServiceSpec(oldImage), dswarm.ServiceCreateOptions{})
	require.NoError(t, err)
	before, err := AgentContainerID(context.Background(), f)
	require.NoError(t, err)
	var log []string
	u, out := newUpgrade(f, &fakeWorkers{log: &log}, &log)
	u.Replace, u.Current = nil, "v1.4.0"
	_, err = u.Do(context.Background())
	require.NoError(t, err)

	assert.Contains(t, out.String(), LeaderReplaceMarker)
	s, _, err := f.ServiceInspectWithRaw(context.Background(), AgentService, dswarm.ServiceInspectOptions{})
	require.NoError(t, err)
	want := LeaderServiceSpec(newImage)
	want.TaskTemplate.ForceUpdate = 1
	assert.Equal(t, want, s.Spec)
	after, err := AgentContainerID(context.Background(), f)
	require.NoError(t, err)
	assert.NotEqual(t, before, after, "a new task: upgrade's check sees a new boot ID")
	assert.NotContains(t, names(f), HelperContainer, "no helper for the service")
}

// A leader still in a plain container is converted straight onto the new
// image: one restart, not a swap and then a conversion.
func TestUpgrade_PlainLeaderIsConvertedOnTheNewImage(t *testing.T) {
	f := leaderEngine(t)
	f.SetNodes(upgradeFake().Nodes())
	var log []string
	u, _ := newUpgrade(f, &fakeWorkers{log: &log}, &log)
	u.Replace, u.Current = nil, "v1.4.0"
	_, err := u.Do(context.Background())
	require.NoError(t, err)
	spec := helperSpec(t, f)
	assert.True(t, spec.ToService)
	assert.Equal(t, newImage, spec.Config.Image)
}

// The first upgrade onto the leader service chains the old swap, its
// settle, the conversion and a cold boot: well past a worker's WaitBack.
func TestWaitReplaced_LeaderWindow(t *testing.T) {
	polls := 0
	ask := func(context.Context) (AgentID, error) {
		polls++
		if polls < int(90*time.Second/pollEvery) {
			return AgentID{}, errors.New("unreachable")
		}
		return AgentID{Version: "v1.5.0", Boot: "new"}, nil
	}
	got, err := WaitReplaced(context.Background(), ask, AgentID{Version: "v1.4.0", Boot: "old"}, "v1.5.0", LeaderWaitBack, noWait)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Boot)
}

// A rebuilt image under the same reference (homelab:dev) is still a new
// agent: the update forces a new task, or Swarm sees nothing to roll out.
func TestUpgrade_LeaderServiceSameImageRefStillRestarts(t *testing.T) {
	f := upgradeFake()
	f.RunServiceTasks()
	_, err := f.ServiceCreate(context.Background(), LeaderServiceSpec(newImage), dswarm.ServiceCreateOptions{})
	require.NoError(t, err)
	before, err := AgentContainerID(context.Background(), f)
	require.NoError(t, err)
	var log []string
	u, _ := newUpgrade(f, &fakeWorkers{log: &log}, &log)
	u.Replace, u.Current = nil, "v1.4.0"
	_, err = u.Do(context.Background())
	require.NoError(t, err)
	after, err := AgentContainerID(context.Background(), f)
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
}
