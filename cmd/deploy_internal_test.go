package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

// fakeSwarm points the per-service lifecycle verbs at an in-memory Swarm.
func fakeSwarm(t *testing.T) *swarmtest.Fake {
	t.Helper()
	f := swarmtest.New()
	f.SetImage("nginx:1", image.InspectResponse{})
	old := newSwarmAPI
	newSwarmAPI = func() (swarm.API, error) { return f, nil }
	t.Cleanup(func() { newSwarmAPI = old })
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

// R20: the core stack still runs on compose for now.
func TestLifecycle_CoreStackStaysOnCompose(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)
	runCLI(t, root, "down")
	runCLI(t, root, "stop")
	assert.Empty(t, f.CallLog())
}
