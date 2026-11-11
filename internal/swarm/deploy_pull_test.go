package swarm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/image"
	dswarm "github.com/docker/docker/api/types/swarm"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// R53: up (and start, restart, edit's apply, a provider's EnsureRunning)
// never upgrades an image. A tag already on the engine is used as it is,
// and the registry is never asked for a newer digest; only pull/update do
// that.
func TestDeploy_UpUsesTheLocalImage(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))

	require.NoError(t, d.Up(ctx, "web"))
	require.NoError(t, d.Up(ctx, "web"))
	assert.Zero(t, count(f.CallLog(), "ImagePull"), "nginx:1 is local: %v", f.CallLog())
	assert.Equal(t, []bool{false, false}, f.RegistryQueries(), "create then update, neither queries the registry")
}

const busyboxCompose = `
networks:
  home-services:
    name: home-services
    external: true
services:
  box:
    image: busybox:1
    container_name: box
    networks: [home-services]
`

// An image that is not on the engine yet is pulled: up must still work on a
// fresh node.
func TestDeploy_UpPullsAMissingImage(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "box", busyboxCompose)
	require.NoError(t, d.Up(ctx, "box"))
	assert.Equal(t, 1, count(f.CallLog(), "ImagePull busybox:1"))
	assert.Equal(t, []bool{false}, f.RegistryQueries())
}

// update is the deliberate upgrade: it pulls and resolves each tag to the
// registry's current digest.
func TestDeploy_UpdatePullsAndQueriesTheRegistry(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Update(ctx, "web"))
	require.NoError(t, d.Update(ctx, "web"))
	assert.Equal(t, 2, count(f.CallLog(), "ImagePull nginx:1"))
	assert.Equal(t, []bool{true, true}, f.RegistryQueries())
}

// A service an earlier update pinned to a digest keeps that digest through
// up: dropping it would roll the tasks onto whatever each node's tag now is.
func TestDeploy_UpKeepsTheDeployedDigest(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))

	pinned := "nginx:1@sha256:" + strings.Repeat("a", 64)
	s := services(t, f, "web")[0]
	spec := s.Spec
	spec.TaskTemplate.ContainerSpec.Image = pinned
	_, err := f.ServiceUpdate(ctx, s.ID, s.Version, spec, dswarm.ServiceUpdateOptions{})
	require.NoError(t, err)

	writeService(t, root, "web", strings.Replace(webCompose, "%s", "baz", 1))
	require.NoError(t, d.Up(ctx, "web"))
	assert.Equal(t, pinned, services(t, f, "web")[0].Spec.TaskTemplate.ContainerSpec.Image)
}

// A new service is pinned to the digest of the image on this engine, when it
// has one, so a task restarting later runs the same image (Swarm's executor
// pulls a bare tag on every task start).
func TestDeploy_UpPinsANewServiceToTheLocalDigest(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	digest := "sha256:" + strings.Repeat("b", 64)
	f.SetImage("nginx:1", image.InspectResponse{
		RepoDigests: []string{"other/nginx@sha256:" + strings.Repeat("c", 64), "nginx@" + digest},
		Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{
			Entrypoint: []string{"/docker-entrypoint.sh"}, Cmd: []string{"nginx"}}},
	})
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))
	assert.Equal(t, "nginx:1@"+digest, services(t, f, "web")[0].Spec.TaskTemplate.ContainerSpec.Image)
	assert.Zero(t, count(f.CallLog(), "ImagePull"))
}
