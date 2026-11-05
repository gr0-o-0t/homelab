package swarm_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/filters"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

const cfToken = "eyJhIjoiYWNjdCIsInQiOiJ0dW4iLCJzIjoic2VjcmV0In0="

func cfDeployer(t *testing.T) (*swarm.Deployer, *swarmtest.Fake, func() []dswarm.Service) {
	t.Helper()
	d, f, _ := newDeployer(t)
	withImage(f, swarm.CloudflaredImage, []string{"cloudflared", "--no-autoupdate"}, []string{"version"})
	list := func() []dswarm.Service {
		l, err := f.ServiceList(context.Background(), dswarm.ServiceListOptions{
			Filters: filters.NewArgs(filters.Arg("label", swarm.LabelService+"=cloudflared"))})
		require.NoError(t, err)
		return l
	}
	return d, f, list
}

func TestCloudflared_TokenNotInSpec(t *testing.T) {
	d, _, list := cfDeployer(t)
	require.NoError(t, d.EnsureCloudflared(context.Background(), cfToken))
	svcs := list()
	require.Len(t, svcs, 1)
	raw, err := json.Marshal(svcs[0].Spec)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), cfToken)
	assert.NotContains(t, string(raw), "TUNNEL_TOKEN")
	cs := svcs[0].Spec.TaskTemplate.ContainerSpec
	assert.Equal(t, []string{"/hl/homelab", "envshim", "--", "cloudflared", "--no-autoupdate", "tunnel", "--no-autoupdate", "run"}, cs.Command)
	require.Len(t, cs.Secrets, 1)
}

func TestCloudflared_PinnedCore(t *testing.T) {
	d, _, list := cfDeployer(t)
	require.NoError(t, d.EnsureCloudflared(context.Background(), cfToken))
	s := list()[0].Spec
	assert.Equal(t, "homelab_cloudflared", s.Name)
	assert.Equal(t, "cloudflared", s.Labels[swarm.LabelService])
	assert.Equal(t, "core", s.Labels[swarm.LabelRole])
	assert.True(t, strings.HasPrefix(s.TaskTemplate.ContainerSpec.Image, "cloudflare/cloudflared:20"), s.TaskTemplate.ContainerSpec.Image)
	assert.NotContains(t, s.TaskTemplate.ContainerSpec.Image, "latest")
	assert.Equal(t, []string{"node.labels.homelab.role==core"}, s.TaskTemplate.Placement.Constraints)
	require.Len(t, s.TaskTemplate.Networks, 1)
	assert.Equal(t, swarm.SharedNetwork, s.TaskTemplate.Networks[0].Target)
	assert.Equal(t, uint64(1), *s.Mode.Replicated.Replicas)
	assert.Equal(t, dswarm.RestartPolicyConditionAny, s.TaskTemplate.RestartPolicy.Condition)
}

func TestCloudflared_RemovedWithoutToken(t *testing.T) {
	d, _, list := cfDeployer(t)
	ctx := context.Background()
	require.NoError(t, d.EnsureCloudflared(ctx, cfToken))
	require.Len(t, list(), 1)
	require.NoError(t, d.EnsureCloudflared(ctx, ""))
	assert.Empty(t, list())
	secs, err := d.API.SecretList(ctx, dswarm.SecretListOptions{})
	require.NoError(t, err)
	assert.Empty(t, secs)
	// Removing what is not there is not an error.
	require.NoError(t, d.RemoveCloudflared(ctx))
}

func TestCloudflared_SameTokenNoUpdate(t *testing.T) {
	d, f, list := cfDeployer(t)
	ctx := context.Background()
	require.NoError(t, d.EnsureCloudflared(ctx, cfToken))
	require.NoError(t, d.EnsureCloudflared(ctx, cfToken))
	n := 0
	for _, c := range f.CallLog() {
		if strings.HasPrefix(c, "ServiceUpdate") {
			n++
		}
	}
	assert.Zero(t, n, "same token must not roll the task")
	require.Len(t, list(), 1)

	require.NoError(t, d.EnsureCloudflared(ctx, cfToken+"x"))
	n = 0
	for _, c := range f.CallLog() {
		if strings.HasPrefix(c, "ServiceUpdate") {
			n++
		}
	}
	assert.Equal(t, 1, n, "a new token updates the service")
	secs, err := d.API.SecretList(ctx, dswarm.SecretListOptions{})
	require.NoError(t, err)
	assert.Len(t, secs, 1, "the old env secret is pruned")
}

func TestCloudflared_OriginIsEdge(t *testing.T) {
	assert.Equal(t, "http://homelab-edge:80", swarm.CloudflaredOrigin)
}
