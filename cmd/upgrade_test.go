package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/remote"
)

// fakeUpgrade stands in for the leader during `homelab upgrade` on a PC:
// run is the forwarded session's result, answers what `version` returns on
// each reconnection attempt (an error: not back yet).
func fakeUpgrade(t *testing.T, version string, code int, runErr error, answers ...any) (*[][]string, *int) {
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
	asks := 0
	upgradeClient = func() (*remote.Client, error) { return &remote.Client{Endpoint: "https://leader:7443"}, nil }
	upgradeRun = func(_ *remote.Client, argv []string) (int, error) {
		forwarded = append(forwarded, argv)
		return code, runErr
	}
	upgradeAsk = func(context.Context, *remote.Client) (string, error) {
		a := answers[min(asks, len(answers)-1)]
		asks++
		if err, ok := a.(error); ok {
			return "", err
		}
		return a.(string), nil
	}
	upgradeSleep = func(context.Context, time.Duration) bool { return true }
	return &forwarded, &asks
}

func TestUpgradeCLI_ForwardsResolvedImageAndVerifiesLeader(t *testing.T) {
	notBack := fmt.Errorf("dial: %w", remote.ErrUnreachable)
	fw, asks := fakeUpgrade(t, "v1.5.0", 1, fmt.Errorf("x: %w", remote.ErrConnectionLost), notBack, "v1.4.0", "v1.5.0")
	code := executeArgs([]string{"upgrade"})
	assert.Equal(t, 0, code)
	require.Len(t, *fw, 1)
	assert.Equal(t, []string{"upgrade", "--image", "ghcr.io/gr0-o-0t/homelab:v1.5.0", "--version", "v1.5.0"}, (*fw)[0],
		"the leader gets this CLI's image, not its own default")
	assert.Equal(t, 3, *asks, "waits while the leader is away or still old")
}

func TestUpgradeCLI_ReportsMismatch(t *testing.T) {
	_, _ = fakeUpgrade(t, "v1.5.0", 0, nil, "v1.4.0")
	assert.Equal(t, 1, executeArgs([]string{"upgrade", "--version", "v1.6.0"}))
}

func TestUpgradeCLI_LeaderNeverBack(t *testing.T) {
	_, asks := fakeUpgrade(t, "v1.5.0", 0, nil, errors.New("dial"))
	assert.Equal(t, 1, executeArgs([]string{"upgrade"}))
	assert.Equal(t, 30, *asks, "60 s of attempts, 2 s apart")
}

// A rollout that halted on the leader (a worker failed) is not waited on.
func TestUpgradeCLI_HaltedRolloutNotVerified(t *testing.T) {
	_, asks := fakeUpgrade(t, "v1.5.0", 1, nil, "v1.5.0")
	assert.Equal(t, 1, executeArgs([]string{"upgrade"}))
	assert.Zero(t, *asks)
}

func TestUpgradeCLI_DevBuildNeedsImage(t *testing.T) {
	fw, _ := fakeUpgrade(t, "v0.0.0-20261110-abc", 0, nil, "x")
	assert.Equal(t, 1, executeArgs([]string{"upgrade"}))
	assert.Empty(t, *fw)

	fw, _ = fakeUpgrade(t, "v0.0.0-20261110-abc", 0, nil, "v0.0.0-20261111-def")
	assert.Equal(t, 0, executeArgs([]string{"upgrade", "--image", "homelab:dev"}))
	assert.Equal(t, []string{"upgrade", "--image", "homelab:dev"}, (*fw)[0], "no expected version: the leader reads the image label")
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
