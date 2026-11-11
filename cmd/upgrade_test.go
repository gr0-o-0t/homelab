package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/agent"
	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

var (
	oldLeader = bootstrap.AgentID{Version: "v1.4.0", Boot: "old-container"}
	newLeader = bootstrap.AgentID{Version: "v1.5.0", Boot: "new-container"}
)

// fakeUpgrade stands in for the leader during `homelab upgrade` on a PC:
// the forwarded session prints the leader-replacement marker (or not) and
// ends with code/runErr; answers are what `version --agent` returns on each
// ask, the first being the one before the upgrade (an error: not there).
func fakeUpgrade(t *testing.T, version string, marker bool, code int, runErr error, answers ...any) (*[][]string, *int, *bytes.Buffer) {
	t.Helper()
	t.Setenv("HOMELAB_AGENT", "") // a PC, not the agent
	prevV, prevRun, prevAsk, prevSleep, prevClient := Version, upgradeRun, upgradeAsk, upgradeSleep, upgradeClient
	t.Cleanup(func() {
		Version, upgradeRun, upgradeAsk, upgradeSleep, upgradeClient = prevV, prevRun, prevAsk, prevSleep, prevClient
		invokedArgs = nil
		resetFlags()
	})
	Version = version
	var forwarded [][]string
	var stream bytes.Buffer
	asks := 0
	upgradeClient = func() (*remote.Client, error) { return &remote.Client{Endpoint: "https://leader:7443"}, nil }
	upgradeRun = func(_ *remote.Client, argv []string, stdout io.Writer) (int, error) {
		forwarded = append(forwarded, argv)
		_, _ = io.WriteString(stdout, "Workers:\n  desktop upgraded\n")
		if marker {
			// split across writes, as a stream may
			_, _ = io.WriteString(stdout, bootstrap.LeaderReplaceMarker[:10])
			_, _ = io.WriteString(stdout, bootstrap.LeaderReplaceMarker[10:]+"; this session ends here.\n")
		}
		return code, runErr
	}
	upgradeAsk = func(context.Context, *remote.Client) (bootstrap.AgentID, error) {
		a := answers[min(asks, len(answers)-1)]
		asks++
		if err, ok := a.(error); ok {
			return bootstrap.AgentID{}, err
		}
		return a.(bootstrap.AgentID), nil
	}
	upgradeSleep = func(context.Context, time.Duration) bool { return true }
	return &forwarded, &asks, &stream
}

var errNotBack = fmt.Errorf("dial: %w", remote.ErrUnreachable)

func TestUpgradeCLI_ForwardsResolvedImageAndVerifiesLeader(t *testing.T) {
	fw, asks, _ := fakeUpgrade(t, "v1.5.0", true, 1, fmt.Errorf("x: %w", remote.ErrConnectionLost),
		oldLeader, errNotBack, oldLeader, newLeader)
	code := executeArgs([]string{"upgrade"})
	assert.Equal(t, 0, code)
	require.Len(t, *fw, 1)
	assert.Equal(t, []string{"upgrade", "--image", "ghcr.io/gr0-o-0t/homelab:v1.5.0", "--version", "v1.5.0"}, (*fw)[0],
		"the leader gets this CLI's image, not its own default")
	assert.Greater(t, *asks, 4, "waits while the leader is away or still old, then for it to stay up")
}

func TestUpgradeCLI_ReportsMismatch(t *testing.T) {
	_, _, _ = fakeUpgrade(t, "v1.5.0", true, 0, nil, oldLeader, newLeader)
	assert.Equal(t, 1, executeArgs([]string{"upgrade", "--version", "v1.6.0"}))
}

func TestUpgradeCLI_LeaderNeverBack(t *testing.T) {
	_, asks, _ := fakeUpgrade(t, "v1.5.0", true, 0, nil, oldLeader, errors.New("dial"))
	assert.Equal(t, 1, executeArgs([]string{"upgrade"}))
	assert.Equal(t, 31, *asks, "the probe before, then 60 s of attempts 2 s apart")
}

// Without a known version (--image only), the old agent still answering —
// its replacement rolled back — is not taken for the new one.
func TestUpgradeCLI_OldLeaderAnsweringIsNotAccepted(t *testing.T) {
	_, _, _ = fakeUpgrade(t, "v1.5.0", true, 0, nil, oldLeader)
	assert.Equal(t, 1, executeArgs([]string{"upgrade", "--image", "homelab:dev"}))
}

// A rollout that halted on the leader (a worker failed) is not waited on.
func TestUpgradeCLI_HaltedRolloutNotVerified(t *testing.T) {
	_, asks, _ := fakeUpgrade(t, "v1.5.0", false, 1, nil, oldLeader, newLeader)
	assert.Equal(t, 1, executeArgs([]string{"upgrade"}))
	assert.Equal(t, 1, *asks, "only the probe before")
}

// A dropped connection before the leader announced its own replacement is
// reported as such, not waited out as a restart.
func TestUpgradeCLI_ConnectionLostBeforeTheMarker(t *testing.T) {
	_, asks, _ := fakeUpgrade(t, "v1.5.0", false, 1, fmt.Errorf("x: %w", remote.ErrConnectionLost), oldLeader, newLeader)
	assert.Equal(t, 1, executeArgs([]string{"upgrade"}))
	assert.Equal(t, 1, *asks)
}

func TestUpgradeCLI_DevBuildNeedsImage(t *testing.T) {
	fw, _, _ := fakeUpgrade(t, "v0.0.0-20261110-abc", true, 0, nil, oldLeader)
	assert.Equal(t, 1, executeArgs([]string{"upgrade"}))
	assert.Empty(t, *fw)

	fw, _, _ = fakeUpgrade(t, "v0.0.0-20261110-abc", true, 0, nil, oldLeader, bootstrap.AgentID{Version: "v0.0.0-20261111-def", Boot: "new"})
	assert.Equal(t, 0, executeArgs([]string{"upgrade", "--image", "homelab:dev"}))
	assert.Equal(t, []string{"upgrade", "--image", "homelab:dev"}, (*fw)[0], "no expected version: the leader reads the image label")
}

// workerScript stands in for the homelab binary a worker agent's remote
// server runs: it answers `version --agent` from files in its state dir
// (which `agent self-replace` rewrites, as a replacement would) and
// `agent self-replace --pull-only`.
const workerScript = `#!/bin/sh
dir=$2; shift 2
[ "$1" = --no-color ] && shift
case "$*" in
  "version --agent") cat "$dir/version"; echo "boot $(cat "$dir/boot")" ;;
  "agent self-replace --image "*" --pull-only") echo pulled ;;
  "agent self-replace --image "*) echo v1.5.0 > "$dir/version"; echo new > "$dir/boot"; echo replacing ;;
  *) echo "unexpected: $*" >&2; exit 9 ;;
esac
`

// The leader's upgrade, through the real worker caller, against a real
// remote server configured as a worker agent's (WorkerResolver over this
// CLI's own command tree, AllowInternal): every command upgrade sends a
// worker must get through.
func TestUpgrade_AgainstARealWorkerAgentServer(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	require.NoError(t, os.MkdirAll(state, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(state, "version"), []byte("v1.4.0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(state, "boot"), []byte("old\n"), 0o600))
	bin := filepath.Join(root, "homelab")
	require.NoError(t, os.WriteFile(bin, []byte(workerScript), 0o700)) //nolint:gosec // a test script

	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	srv := httptest.NewTLSServer(remote.NewServer(remote.ServerConfig{
		StateDir: state, StateRoot: root, Secret: secret, Version: "v1.4.0", Binary: bin,
		Resolve:       agent.WorkerResolver(ResolveCommand),
		AllowInternal: map[string]bool{agent.SelfReplacePath: true},
		Logf:          func(string, ...any) {},
	}).Handler())
	t.Cleanup(srv.Close)
	tlsConfig := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()

	prev := Version
	Version = "v1.4.0" // the leader's (old) agent makes these calls
	t.Cleanup(func() { Version = prev })

	f := swarmtest.New()
	f.SetImage("ghcr.io/gr0-o-0t/homelab:v1.5.0", imageLabelled("v1.5.0"))
	f.SetNodes([]dswarm.Node{
		{ID: "n0", Spec: dswarm.NodeSpec{Role: dswarm.NodeRoleManager}, Status: dswarm.NodeStatus{State: dswarm.NodeStateReady}},
		{ID: "n1", Spec: dswarm.NodeSpec{Role: dswarm.NodeRoleWorker}, Description: dswarm.NodeDescription{Hostname: "desktop"},
			Status: dswarm.NodeStatus{State: dswarm.NodeStateReady}},
	})
	var out bytes.Buffer
	leaderReplaced := false
	u := &bootstrap.Upgrade{
		API: f, Image: "ghcr.io/gr0-o-0t/homelab:v1.5.0", Out: &out,
		Call:    newWorkerCall(func(string) string { return srv.URL }, secret, tlsConfig),
		Replace: func(context.Context) error { leaderReplaced = true; return nil },
		Sleep:   func(context.Context, time.Duration) bool { return true },
	}
	res, err := u.Do(context.Background())
	require.NoError(t, err, out.String())
	require.Len(t, res, 1)
	assert.Equal(t, bootstrap.WorkerUpgraded, res[0].Status, out.String())
	assert.Equal(t, "v1.5.0", res[0].Version)
	assert.True(t, leaderReplaced)
	assert.False(t, strings.Contains(out.String(), "unknown command"), out.String())
}

// The leader's agent is replaced only as upgrade's last step: asked
// directly, self-replace refuses there; on a worker it hands the
// replacement to the helper container.
func TestAgentSelfReplace_RefusesOnTheLeader(t *testing.T) {
	t.Cleanup(resetFlags)
	f := fakeSwarm(t)
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, ControlAvailable: true}})
	assert.Equal(t, 1, executeArgs([]string{"agent", "self-replace", "--image", "nginx:1"}))
	assert.Empty(t, f.Created())

	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive}})
	require.NoError(t, bootstrap.StartAgent(context.Background(), f, "nginx:1", false))
	assert.Equal(t, 0, executeArgs([]string{"agent", "self-replace", "--image", "nginx:1", "--pull-only"}))
	assert.Len(t, f.Created(), 1, "--pull-only replaces nothing")
	resetFlags()
	assert.Equal(t, 0, executeArgs([]string{"agent", "self-replace", "--image", "nginx:1"}))
	assert.Equal(t, bootstrap.HelperContainer, f.Created()[1].Name)
}

func imageLabelled(version string) image.InspectResponse {
	return image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{
		Labels: map[string]string{bootstrap.VersionLabel: version}}}}
}
