package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dswarm "github.com/docker/docker/api/types/swarm"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

// fakeSwarm points the per-service lifecycle verbs at an in-memory Swarm.
func fakeSwarm(t *testing.T) *swarmtest.Fake {
	t.Helper()
	f := swarmtest.New()
	f.SetImage("nginx:1", image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{Cmd: []string{"nginx"}},
	}})
	old, oldSecrets := newSwarmAPI, openSecrets
	newSwarmAPI = func() (swarm.API, error) { return f, nil }
	sm := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	openSecrets = func() (*secrets.Manager, error) { return sm, nil }
	t.Cleanup(func() { newSwarmAPI, openSecrets = old, oldSecrets })
	return f
}

func webFixture(t *testing.T) string {
	t.Helper()
	root := desiredFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "services", "alpha", "docker-compose.yml"), []byte(`
services:
  web:
    image: nginx:1
    environment:
      FOO: bar
`), 0o644))
	return root
}

func alphaServices(t *testing.T, f *swarmtest.Fake) []dswarm.Service {
	t.Helper()
	list, err := f.ServiceList(context.Background(), dswarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", swarm.LabelService+"=alpha")),
	})
	require.NoError(t, err)
	return list
}

func calls(f *swarmtest.Fake, prefix string) int {
	n := 0
	for _, c := range f.CallLog() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func replicasOf(s dswarm.Service) uint64 {
	return *s.Spec.Mode.Replicated.Replicas
}

func TestLifecycle_ServiceVerbsRunOnSwarm(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)

	runCLI(t, root, "up", "alpha")
	svcs := alphaServices(t, f)
	require.Len(t, svcs, 1)
	assert.Equal(t, "alpha_web", svcs[0].Spec.Name)
	assert.Equal(t, uint64(1), replicasOf(svcs[0]))
	assert.Equal(t, 1, calls(f, "ServiceCreate alpha_web"))
	assert.Equal(t, 1, calls(f, "ImagePull nginx:1"))

	runCLI(t, root, "stop", "alpha")
	assert.Equal(t, uint64(0), replicasOf(alphaServices(t, f)[0]), "stop scales to zero")

	runCLI(t, root, "start", "alpha")
	assert.Equal(t, uint64(1), replicasOf(alphaServices(t, f)[0]), "start is up")

	runCLI(t, root, "restart", "alpha")
	assert.Equal(t, uint64(1), alphaServices(t, f)[0].Spec.TaskTemplate.ForceUpdate)

	runCLI(t, root, "down", "alpha")
	assert.Equal(t, uint64(0), replicasOf(alphaServices(t, f)[0]), "down keeps the spec at zero")

	runCLI(t, root, "pull", "alpha")
	assert.Equal(t, 3, calls(f, "ImagePull nginx:1"), "up, start and pull each pulled")

	runCLI(t, root, "update", "alpha")
	assert.Equal(t, uint64(1), replicasOf(alphaServices(t, f)[0]))
	assert.Equal(t, 1, calls(f, "ServiceCreate"), "never re-created")

	runCLI(t, root, "delete", "--yes", "alpha")
	assert.Empty(t, alphaServices(t, f))
	assert.NoDirExists(t, filepath.Join(root, "services", "alpha"))
	secs, err := f.SecretList(context.Background(), dswarm.SecretListOptions{})
	require.NoError(t, err)
	assert.Empty(t, secs, "delete removes the env secrets")
}

func TestLifecycle_RestartAfterStopRuns(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)
	runCLI(t, root, "up", "alpha")
	runCLI(t, root, "stop", "alpha")
	require.Equal(t, uint64(0), replicasOf(alphaServices(t, f)[0]))
	runCLI(t, root, "restart", "alpha")
	assert.Equal(t, uint64(1), replicasOf(alphaServices(t, f)[0]), "restart after stop runs the service")
	assert.Equal(t, desired.Running, loadDesired(t, root).Services["alpha"])
}

// R23: without the secrets manager, verbs that start containers refuse;
// stopping still works.
func TestLifecycle_NoSecretsManagerRefusesDeploy(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)
	runCLI(t, root, "up", "alpha")
	openSecrets = func() (*secrets.Manager, error) { return nil, errors.New("vault locked") }

	before := len(f.CallLog())
	for _, verb := range []string{"up", "start", "restart", "update"} {
		resetFlags()
		rootCmd.SetArgs([]string{"--config-dir", root, verb, "alpha"})
		err := rootCmd.Execute()
		require.Error(t, err, verb)
		assert.Contains(t, err.Error(), "vault locked", verb)
	}
	resetFlags()
	assert.Len(t, f.CallLog(), before, "no Swarm call made")

	runCLI(t, root, "stop", "alpha")
	assert.Equal(t, uint64(0), replicasOf(alphaServices(t, f)[0]), "stop still works")
}

// With no service, the lifecycle verbs refuse before touching Swarm.
func TestLifecycle_NoServiceMakesNoSwarmCall(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)
	runCLI(t, root, "down")
	runCLI(t, root, "stop")
	assert.Empty(t, f.CallLog())
}
