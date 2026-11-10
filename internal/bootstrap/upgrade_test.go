package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

const (
	oldImage = "ghcr.io/gr0-o-0t/homelab:v1.4.0"
	newImage = "ghcr.io/gr0-o-0t/homelab:v1.5.0"
)

func imageWith(version string, env ...string) image.InspectResponse {
	return image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{
		Env:    env,
		Labels: map[string]string{VersionLabel: version},
	}}}
}

// fakeWorkers is a cluster of worker agents reached over the tailnet: each
// answers `version`, `agent self-replace --pull-only` and `agent
// self-replace`, and comes back on the new version once replaced.
type fakeWorkers struct {
	mu          sync.Mutex
	calls       []string
	version     map[string]string
	unreachable map[string]bool
	failPull    map[string]bool
	staysOld    map[string]bool
	log         *[]string // shared with the leader's replace, for ordering
}

func (w *fakeWorkers) call(_ context.Context, host string, argv []string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := host + ": " + strings.Join(argv, " ")
	w.calls = append(w.calls, line)
	if w.log != nil {
		*w.log = append(*w.log, line)
	}
	if w.unreachable[host] {
		return "", fmt.Errorf("connect to the agent at homelab-%s: dial: %w", host, remote.ErrUnreachable)
	}
	switch {
	case slices.Equal(argv, []string{"version"}):
		return w.version[host] + "\n", nil
	case slices.Contains(argv, "--pull-only"):
		if w.failPull[host] {
			return "", errors.New("pulling: manifest unknown")
		}
		return "pulled\n", nil
	case len(argv) >= 2 && argv[0] == "agent" && argv[1] == "self-replace":
		if !w.staysOld[host] {
			w.version[host] = "v1.5.0"
		}
		return "replacing\n", nil
	}
	return "", fmt.Errorf("unexpected %v", argv)
}

func workerNode(id, host string, state dswarm.NodeState) dswarm.Node {
	return dswarm.Node{ID: id,
		Spec:        dswarm.NodeSpec{Role: dswarm.NodeRoleWorker},
		Description: dswarm.NodeDescription{Hostname: host},
		Status:      dswarm.NodeStatus{State: state}}
}

func upgradeFake(nodes ...dswarm.Node) *swarmtest.Fake {
	f := swarmtest.New()
	f.SetImage(newImage, imageWith("v1.5.0"))
	leader := dswarm.Node{ID: "n0", Spec: dswarm.NodeSpec{Role: dswarm.NodeRoleManager},
		Description: dswarm.NodeDescription{Hostname: "minis"}, Status: dswarm.NodeStatus{State: dswarm.NodeStateReady}}
	f.SetNodes(append([]dswarm.Node{leader}, nodes...))
	return f
}

func newUpgrade(f *swarmtest.Fake, w *fakeWorkers, log *[]string) (*Upgrade, *bytes.Buffer) {
	var out bytes.Buffer
	return &Upgrade{
		API:   f,
		Image: newImage,
		Call:  w.call,
		Replace: func(context.Context) error {
			*log = append(*log, "leader: self-replace")
			return nil
		},
		Out:   &out,
		Sleep: func(context.Context, time.Duration) bool { return true },
	}, &out
}

func TestUpgrade_WorkersBeforeLeader(t *testing.T) {
	f := upgradeFake(workerNode("n2", "laptop", dswarm.NodeStateReady), workerNode("n1", "desktop", dswarm.NodeStateReady))
	var log []string
	w := &fakeWorkers{version: map[string]string{"laptop": "v1.4.0", "desktop": "v1.4.0"}, log: &log}
	u, out := newUpgrade(f, w, &log)

	res, err := u.Do(context.Background())
	require.NoError(t, err, out.String())

	// Every node pulls before any agent is replaced; workers one by one
	// (each back on the new version before the next), the leader last.
	pulls := []string{
		"desktop: agent self-replace --image " + newImage + " --pull-only",
		"laptop: agent self-replace --image " + newImage + " --pull-only",
	}
	var order []string
	for _, l := range log {
		if strings.Contains(l, "self-replace") {
			order = append(order, l)
		}
	}
	assert.Equal(t, append(pulls,
		"desktop: agent self-replace --image "+newImage,
		"laptop: agent self-replace --image "+newImage,
		"leader: self-replace"), order)
	assert.Contains(t, f.CallLog(), "ImageInspectWithRaw "+newImage, "the leader's own engine has the image first")

	require.Len(t, res, 2)
	for _, r := range res {
		assert.Equal(t, WorkerUpgraded, r.Status, r.Host)
		assert.Equal(t, "v1.5.0", r.Version, r.Host)
	}
	assert.Contains(t, out.String(), "desktop")
	assert.Contains(t, out.String(), "agent restarting")
}

func TestUpgrade_OfflineWorkerSkipped(t *testing.T) {
	f := upgradeFake(
		workerNode("n1", "laptop", dswarm.NodeStateDown),   // asleep: swarm knows
		workerNode("n2", "desktop", dswarm.NodeStateReady), // reachable
		workerNode("n3", "tablet", dswarm.NodeStateReady),  // swarm not caught up yet
	)
	var log []string
	w := &fakeWorkers{version: map[string]string{"desktop": "v1.4.0", "tablet": "v1.4.0"},
		unreachable: map[string]bool{"tablet": true}, log: &log}
	u, out := newUpgrade(f, w, &log)

	res, err := u.Do(context.Background())
	require.NoError(t, err, out.String())
	got := map[string]WorkerStatus{}
	for _, r := range res {
		got[r.Host] = r.Status
	}
	assert.Equal(t, map[string]WorkerStatus{"laptop": WorkerSkipped, "desktop": WorkerUpgraded, "tablet": WorkerSkipped}, got)
	for _, c := range w.calls {
		assert.NotContains(t, c, "laptop:", "a down node is not dialled")
	}
	assert.Equal(t, "leader: self-replace", log[len(log)-1], "skipped workers do not hold back the leader")
	assert.Contains(t, out.String(), "skipped")
}

// A failed worker (its pull, or its agent coming back on another version)
// halts the rollout before the leader: rerunning finishes it.
func TestUpgrade_FailedWorkerHaltsBeforeLeader(t *testing.T) {
	for name, w := range map[string]*fakeWorkers{
		"pull":     {version: map[string]string{"desktop": "v1.4.0"}, failPull: map[string]bool{"desktop": true}},
		"mismatch": {version: map[string]string{"desktop": "v1.4.0"}, staysOld: map[string]bool{"desktop": true}},
	} {
		t.Run(name, func(t *testing.T) {
			f := upgradeFake(workerNode("n1", "desktop", dswarm.NodeStateReady))
			var log []string
			w.log = &log
			u, out := newUpgrade(f, w, &log)
			res, err := u.Do(context.Background())
			require.Error(t, err)
			assert.NotContains(t, log, "leader: self-replace")
			require.Len(t, res, 1)
			assert.Equal(t, WorkerFailed, res[0].Status)
			if name == "mismatch" {
				assert.Contains(t, out.String(), "v1.4.0")
				assert.Contains(t, res[0].Detail, "expected v1.5.0")
			}
		})
	}
}

// A worker already on the target version (a rerun after a halted rollout)
// is not replaced again.
func TestUpgrade_WorkerAlreadyCurrent(t *testing.T) {
	f := upgradeFake(workerNode("n1", "desktop", dswarm.NodeStateReady))
	var log []string
	w := &fakeWorkers{version: map[string]string{"desktop": "v1.5.0"}, log: &log}
	u, _ := newUpgrade(f, w, &log)
	res, err := u.Do(context.Background())
	require.NoError(t, err)
	assert.Equal(t, WorkerCurrent, res[0].Status)
	for _, c := range w.calls {
		assert.NotContains(t, c, "self-replace", "nothing to pull or replace")
	}
}

// startedAgent is a fake engine running the agent container as init/join
// created it, from oldImage.
func startedAgent(t *testing.T, leader bool) *swarmtest.Fake {
	t.Helper()
	f := swarmtest.New()
	f.SetImage(oldImage, imageWith("v1.4.0", "PATH=/usr/local/bin:/usr/bin:/bin"))
	f.SetImage(newImage, imageWith("v1.5.0", "PATH=/usr/local/bin:/usr/bin:/bin"))
	f.SetVolumes(&volume.Volume{Name: StateVolume}, &volume.Volume{Name: ShimVolume},
		&volume.Volume{Name: agentstate.BackupsVolume}, &volume.Volume{Name: "immich_data"})
	require.NoError(t, StartAgent(context.Background(), f, oldImage, leader))
	return f
}

func created(t *testing.T, f *swarmtest.Fake, name string) swarmtest.CreatedContainer {
	t.Helper()
	for _, c := range f.Created() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %s", name)
	return swarmtest.CreatedContainer{}
}

func helperSpec(t *testing.T, f *swarmtest.Fake) ReplaceSpec {
	t.Helper()
	h := created(t, f, HelperContainer)
	for _, e := range h.Config.Env {
		if v, ok := strings.CutPrefix(e, ReplaceSpecEnv+"="); ok {
			var s ReplaceSpec
			require.NoError(t, json.Unmarshal([]byte(v), &s))
			return s
		}
	}
	t.Fatal("the helper has no replace spec")
	return ReplaceSpec{}
}

// replace runs the whole replacement on f: what self-replace starts, then
// what the helper container does.
func replace(t *testing.T, f *swarmtest.Fake) {
	t.Helper()
	require.NoError(t, SelfReplace(context.Background(), f, newImage, &bytes.Buffer{}))
	spec := helperSpec(t, f)
	require.NoError(t, RunReplace(context.Background(), f, spec, func(context.Context, time.Duration) bool { return true }))
}

func TestUpgrade_PreservesMountsAndAlias(t *testing.T) {
	for _, leader := range []bool{true, false} {
		t.Run(fmt.Sprintf("leader=%v", leader), func(t *testing.T) {
			f := startedAgent(t, leader)
			before := created(t, f, AgentContainer)
			replace(t, f)
			after := created(t, f, AgentContainer)

			assert.True(t, after.Started)
			assert.Equal(t, newImage, after.Config.Image)
			assert.Equal(t, before.HostConfig.Binds, after.HostConfig.Binds, "every mount kept")
			assert.Equal(t, before.HostConfig.RestartPolicy, after.HostConfig.RestartPolicy)
			assert.Equal(t, before.Config.Env, after.Config.Env)
			assert.Equal(t, before.Config.Cmd, after.Config.Cmd)
			assert.Equal(t, before.Config.Labels, after.Config.Labels)
			if leader {
				ep := after.Networking.EndpointsConfig[swarm.SharedNetwork]
				require.NotNil(t, ep, "still on home-services")
				assert.Equal(t, []string{EdgeAlias}, ep.Aliases)
			} else {
				assert.Empty(t, after.Networking.EndpointsConfig[swarm.SharedNetwork])
			}
		})
	}
}

func TestUpgrade_NeverTouchesVolumes(t *testing.T) {
	f := startedAgent(t, true)
	replace(t, f)
	for _, c := range f.CallLog() {
		assert.False(t, strings.HasPrefix(c, "Volume") && !strings.HasPrefix(c, "VolumeList"), "volume call %q", c)
	}
	removed := f.Removed()
	require.NotEmpty(t, removed)
	for _, r := range removed {
		assert.False(t, r.Options.RemoveVolumes, "%s removed with its volumes", r.Name)
	}
	vols, err := f.VolumeList(context.Background(), volume.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, vols.Volumes, 4)
}

func TestSelfReplace_HelperSpecMatchesOldContainer(t *testing.T) {
	f := startedAgent(t, true)
	before := created(t, f, AgentContainer)
	require.NoError(t, SelfReplace(context.Background(), f, newImage, &bytes.Buffer{}))

	h := created(t, f, HelperContainer)
	assert.True(t, h.Started)
	assert.Equal(t, newImage, h.Config.Image, "the helper runs the new image")
	assert.Equal(t, []string{"agent", "replace-helper"}, []string(h.Config.Cmd))
	assert.Equal(t, []string{"/var/run/docker.sock:/var/run/docker.sock"}, h.HostConfig.Binds, "the docker socket only")
	assert.True(t, h.HostConfig.AutoRemove, "the helper is removed when it exits")

	s := helperSpec(t, f)
	assert.Equal(t, AgentContainer, s.Container)
	assert.Equal(t, oldImage, s.OldImage, "kept for a rollback")
	want := before.Config
	want.Image = newImage
	assert.Equal(t, want, s.Config, "the old container's config, image swapped; the image's own env not frozen in")
	assert.Equal(t, before.HostConfig, s.HostConfig)
	assert.Equal(t, []string{EdgeAlias}, s.Networking.EndpointsConfig[swarm.SharedNetwork].Aliases)
	assert.Positive(t, s.Delay, "the old agent gets to finish answering first")

	// The old agent is still running: the helper replaces it, not self-replace.
	assert.True(t, created(t, f, AgentContainer).Started)
	assert.NotContains(t, f.CallLog(), "ContainerStop "+AgentContainer)
}

// A new container that does not start is rolled back to the old image.
func TestRunReplace_RollsBackWhenTheNewAgentFails(t *testing.T) {
	f := startedAgent(t, false)
	require.NoError(t, SelfReplace(context.Background(), f, newImage, &bytes.Buffer{}))
	spec := helperSpec(t, f)
	f.FailStart(newImage, errors.New("exec format error"))
	err := RunReplace(context.Background(), f, spec, func(context.Context, time.Duration) bool { return true })
	require.Error(t, err)
	after := created(t, f, AgentContainer)
	assert.Equal(t, oldImage, after.Config.Image)
	assert.True(t, after.Started)
}

func TestReplacementSpec_DropsImageDefaults(t *testing.T) {
	f := startedAgent(t, false)
	// Docker reports the container's env and labels merged with its image's.
	f.MergeImageDefaults(AgentContainer, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, map[string]string{VersionLabel: "v1.4.0"})
	s, err := ReplacementSpec(context.Background(), f, AgentContainer, newImage)
	require.NoError(t, err)
	assert.NotContains(t, s.Config.Env, "PATH=/usr/local/bin:/usr/bin:/bin")
	assert.NotContains(t, s.Config.Labels, VersionLabel)
	assert.Contains(t, s.Config.Env, "XDG_DATA_HOME="+agentstate.Root+"/xdg/data")
}
