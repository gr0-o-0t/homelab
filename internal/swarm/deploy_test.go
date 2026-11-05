package swarm_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/envshim"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

const webCompose = `
networks:
  home-services:
    name: home-services
    external: true
services:
  web:
    image: nginx:1
    container_name: web
    restart: always
    environment:
      FOO: %s
    networks: [home-services]
`

const giteaCompose = `
networks:
  home-services:
    name: home-services
    external: true
services:
  gitea:
    image: gitea/gitea:1
    container_name: gitea
    restart: always
    volumes:
      - gitea-data:/var/lib/gitea
    networks: [home-services]
volumes:
  gitea-data:
    name: gitea_data
`

// writeService installs services/<svc>/docker-compose.yml under root.
func writeService(t *testing.T, root, svc, compose string) {
	t.Helper()
	dir := filepath.Join(root, "services", svc)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644))
}

// withImage registers the entrypoint/cmd the fake returns when ref is inspected.
func withImage(f *swarmtest.Fake, ref string, entry, cmd []string) {
	f.SetImage(ref, image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{Entrypoint: entry, Cmd: cmd},
	}})
}

func newDeployer(t *testing.T) (*swarm.Deployer, *swarmtest.Fake, string) {
	t.Helper()
	f := swarmtest.New()
	withImage(f, "nginx:1", []string{"/docker-entrypoint.sh"}, []string{"nginx", "-g", "daemon off;"})
	withImage(f, "gitea/gitea:1", nil, []string{"/usr/bin/entrypoint"})
	root := t.TempDir()
	sm := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	return &swarm.Deployer{API: f, Root: root, SM: sm}, f, root
}

func node(id string, role string, state dswarm.NodeState, avail dswarm.NodeAvailability) dswarm.Node {
	return dswarm.Node{
		ID:          id,
		Spec:        dswarm.NodeSpec{Annotations: dswarm.Annotations{Labels: map[string]string{"homelab.role": role}}, Availability: avail},
		Description: dswarm.NodeDescription{Hostname: id},
		Status:      dswarm.NodeStatus{State: state},
	}
}

func services(t *testing.T, f *swarmtest.Fake, svc string) []dswarm.Service {
	t.Helper()
	list, err := f.ServiceList(context.Background(), dswarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", swarm.LabelService+"="+svc)),
	})
	require.NoError(t, err)
	return list
}

func secretsOf(t *testing.T, f *swarmtest.Fake, svc string) []dswarm.Secret {
	t.Helper()
	list, err := f.SecretList(context.Background(), dswarm.SecretListOptions{
		Filters: filters.NewArgs(filters.Arg("label", swarm.LabelService+"="+svc)),
	})
	require.NoError(t, err)
	return list
}

func count(log []string, prefix string) int {
	n := 0
	for _, c := range log {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func replicas(s dswarm.Service) uint64 {
	if s.Spec.Mode.Replicated == nil || s.Spec.Mode.Replicated.Replicas == nil {
		return 999
	}
	return *s.Spec.Mode.Replicated.Replicas
}

func TestDeploy_UpCreatesThenUpdates(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))

	require.NoError(t, d.Up(ctx, "web"))
	svcs := services(t, f, "web")
	require.Len(t, svcs, 1)
	s := svcs[0]
	assert.Equal(t, "web_web", s.Spec.Name)
	assert.Equal(t, uint64(1), replicas(s))
	assert.Equal(t, "worker", s.Spec.Labels[swarm.LabelRole])
	cs := s.Spec.TaskTemplate.ContainerSpec
	assert.Equal(t, []string{"/hl/homelab", "envshim", "--", "/docker-entrypoint.sh", "nginx", "-g", "daemon off;"}, cs.Command)
	assert.Empty(t, cs.Env, "env goes through a secret, never the spec")

	content := envshim.Encode(map[string]string{"FOO": "bar"})
	name := envshim.SecretName("web_web", content)
	require.Len(t, cs.Secrets, 1)
	assert.Equal(t, name, cs.Secrets[0].SecretName)
	assert.NotEmpty(t, cs.Secrets[0].SecretID, "Swarm needs the secret's ID in the reference")
	secs := secretsOf(t, f, "web")
	require.Len(t, secs, 1)
	assert.Equal(t, name, secs[0].Spec.Name)
	assert.Equal(t, content, secs[0].Spec.Data)
	assert.Contains(t, f.CallLog(), "ImagePull nginx:1")

	require.NoError(t, d.Up(ctx, "web"))
	log := f.CallLog()
	assert.Equal(t, 1, count(log, "ServiceCreate"), "second up updates, never re-creates")
	assert.Equal(t, 1, count(log, "ServiceUpdate"))
	assert.Equal(t, 1, count(log, "SecretCreate"), "an unchanged env reuses its secret")
	assert.Len(t, services(t, f, "web"), 1)
}

func TestDeploy_DownScalesToZeroKeepsVolumes(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "gitea", giteaCompose)
	f.SetVolumes(&volume.Volume{Name: "gitea_data"})

	require.NoError(t, d.Up(ctx, "gitea"))
	require.NoError(t, d.Down(ctx, "gitea"))

	svcs := services(t, f, "gitea")
	require.Len(t, svcs, 1, "down keeps the spec")
	assert.Equal(t, uint64(0), replicas(svcs[0]))
	vols, err := f.VolumeList(ctx, volume.ListOptions{})
	require.NoError(t, err)
	require.Len(t, vols.Volumes, 1)
	for _, c := range f.CallLog() {
		assert.NotContains(t, c, "Remove", "down removes nothing")
	}

	require.NoError(t, d.Up(ctx, "gitea"))
	assert.Equal(t, uint64(1), replicas(services(t, f, "gitea")[0]), "up after down scales back to one")
}

func TestDeploy_EnsureNetworkValues(t *testing.T) {
	ctx := context.Background()
	d, f, _ := newDeployer(t)
	require.NoError(t, d.EnsureNetwork(ctx))
	require.NoError(t, d.EnsureNetwork(ctx))
	assert.Equal(t, 1, count(f.CallLog(), "NetworkCreate"), "idempotent")

	nets, err := f.NetworkList(ctx, network.ListOptions{})
	require.NoError(t, err)
	require.Len(t, nets, 1)
	n := nets[0]
	assert.Equal(t, "home-services", n.Name)
	assert.Equal(t, "overlay", n.Driver)
	assert.Equal(t, "swarm", n.Scope)
	assert.True(t, n.Attachable)
	assert.Equal(t, map[string]string{"com.docker.network.driver.mtu": "1230"}, n.Options, "MTU 1230 and not encrypted")
	_, encrypted := n.Options["encrypted"]
	assert.False(t, encrypted)
}

func TestDeploy_EnsureNetworkRefusesComposeBridge(t *testing.T) {
	ctx := context.Background()
	d, f, _ := newDeployer(t)
	_, err := f.NetworkCreate(ctx, "home-services", network.CreateOptions{Driver: "bridge"})
	require.NoError(t, err)
	err = d.EnsureNetwork(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge")
	assert.Contains(t, err.Error(), "homelab down")
}

func TestDeploy_StatelessOnMinisWhenNoWorker(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	f.SetNodes([]dswarm.Node{
		node("mini1", "core", dswarm.NodeStateReady, dswarm.NodeAvailabilityActive),
		node("laptop", "worker", dswarm.NodeStateDown, dswarm.NodeAvailabilityActive),
		node("desktop", "worker", dswarm.NodeStateReady, dswarm.NodeAvailabilityDrain),
	})
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))
	p := services(t, f, "web")[0].Spec.TaskTemplate.Placement
	if p != nil {
		assert.Empty(t, p.Constraints, "no ready worker: run anywhere, i.e. on the minis")
	}
}

func TestDeploy_StatelessConstrainedToWorkerWhenReady(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	f.SetNodes([]dswarm.Node{
		node("mini1", "core", dswarm.NodeStateReady, dswarm.NodeAvailabilityActive),
		node("laptop", "worker", dswarm.NodeStateReady, dswarm.NodeAvailabilityActive),
	})
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	writeService(t, root, "gitea", giteaCompose)
	require.NoError(t, d.Up(ctx, "web"))
	require.NoError(t, d.Up(ctx, "gitea"))

	web := services(t, f, "web")[0].Spec.TaskTemplate.Placement
	require.NotNil(t, web)
	assert.Equal(t, []string{"node.labels.homelab.role==worker"}, web.Constraints)
	gitea := services(t, f, "gitea")[0].Spec.TaskTemplate.Placement
	require.NotNil(t, gitea)
	assert.Equal(t, []string{"node.labels.homelab.role==core"}, gitea.Constraints, "a data volume pins to core")
}

func TestDeploy_StaleEnvSecretRemoved(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))
	old := secretsOf(t, f, "web")[0].Spec.Name

	writeService(t, root, "web", strings.Replace(webCompose, "%s", "baz", 1))
	require.NoError(t, d.Up(ctx, "web"))
	secs := secretsOf(t, f, "web")
	require.Len(t, secs, 1, "the old env secret is pruned")
	assert.NotEqual(t, old, secs[0].Spec.Name)
	ref := services(t, f, "web")[0].Spec.TaskTemplate.ContainerSpec.Secrets
	require.Len(t, ref, 1)
	assert.Equal(t, secs[0].Spec.Name, ref[0].SecretName)
}

func TestDeploy_RemoveKeepsVolumes(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "gitea", giteaCompose)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	f.SetVolumes(&volume.Volume{Name: "gitea_data"})
	require.NoError(t, d.Up(ctx, "gitea"))
	require.NoError(t, d.Up(ctx, "web"))

	require.NoError(t, d.Remove(ctx, "web"))
	require.NoError(t, d.Remove(ctx, "gitea"))
	assert.Empty(t, services(t, f, "gitea"))
	assert.Empty(t, services(t, f, "web"))
	assert.Empty(t, secretsOf(t, f, "web"))
	vols, err := f.VolumeList(ctx, volume.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, vols.Volumes, 1, "volumes are never deleted")
}

func TestDeploy_RestartForcesUpdate(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))
	before := services(t, f, "web")[0].Spec.TaskTemplate.ForceUpdate
	require.NoError(t, d.Restart(ctx, "web"))
	assert.Equal(t, before+1, services(t, f, "web")[0].Spec.TaskTemplate.ForceUpdate)
}

func TestDeploy_RestartRevivesStoppedService(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))
	require.NoError(t, d.Down(ctx, "web"))
	require.NoError(t, d.Restart(ctx, "web"))
	s := services(t, f, "web")[0]
	assert.Equal(t, uint64(1), replicas(s), "restart means running afterwards")
	assert.Equal(t, uint64(1), s.Spec.TaskTemplate.ForceUpdate)
}

// R23: no secrets manager, no deploy — Up would rotate a working env secret
// to one missing every secret. Taking things down stays possible.
func TestDeploy_RefusesWithoutSecretsManager(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))

	d.SM = nil
	before := len(f.CallLog())
	require.ErrorIs(t, d.Up(ctx, "web"), swarm.ErrNoSecrets)
	require.ErrorIs(t, d.Update(ctx, "web"), swarm.ErrNoSecrets)
	require.ErrorIs(t, d.Restart(ctx, "web"), swarm.ErrNoSecrets)
	assert.Len(t, f.CallLog(), before, "nothing touched")
	assert.Contains(t, swarm.ErrNoSecrets.Error(), "keyring")

	require.NoError(t, d.Down(ctx, "web"))
	require.NoError(t, d.Pull(ctx, "web"))
	require.NoError(t, d.Remove(ctx, "web"))
}

func TestDeploy_RemoveDeletesStackNetworks(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", `
networks:
  home-services:
    name: home-services
    external: true
  internal:
    internal: true
services:
  web:
    image: nginx:1
    networks: [home-services, internal]
`)
	require.NoError(t, d.Up(ctx, "web"))
	nets, err := f.NetworkList(ctx, network.ListOptions{})
	require.NoError(t, err)
	require.Len(t, nets, 2)

	require.NoError(t, d.Remove(ctx, "web"))
	nets, err = f.NetworkList(ctx, network.ListOptions{})
	require.NoError(t, err)
	require.Len(t, nets, 1)
	assert.Equal(t, "home-services", nets[0].Name, "the shared overlay is never removed")
}

func TestDeploy_UpRefusesImageWithoutCommand(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	f.SetImage("bare:1", image.InspectResponse{})
	writeService(t, root, "bare", `
services:
  bare:
    image: bare:1
    environment:
      FOO: bar
`)
	err := d.Up(ctx, "bare")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bare_bare")
	assert.Contains(t, err.Error(), "bare:1")
	assert.Empty(t, services(t, f, "bare"))
}

// racingAPI updates the service behind the deployer's back on the first
// ServiceUpdate, so the deployer's version is stale exactly once.
type racingAPI struct {
	*swarmtest.Fake
	raced bool
}

func (r *racingAPI) ServiceUpdate(ctx context.Context, id string, v dswarm.Version, spec dswarm.ServiceSpec,
	o dswarm.ServiceUpdateOptions,
) (dswarm.ServiceUpdateResponse, error) {
	if !r.raced {
		r.raced = true
		cur, _, err := r.ServiceInspectWithRaw(ctx, id, dswarm.ServiceInspectOptions{})
		if err != nil {
			return dswarm.ServiceUpdateResponse{}, err
		}
		other := cur.Spec
		other.Labels = map[string]string{"raced": "yes"}
		if _, err := r.Fake.ServiceUpdate(ctx, id, cur.Version, other, o); err != nil {
			return dswarm.ServiceUpdateResponse{}, err
		}
	}
	return r.Fake.ServiceUpdate(ctx, id, v, spec, o)
}

func TestDeploy_UpRetriesStaleVersion(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))

	d.API = &racingAPI{Fake: f}
	require.NoError(t, d.Up(ctx, "web"), "a stale version is re-inspected and retried once")
	s := services(t, f, "web")[0]
	assert.Equal(t, "web", s.Spec.Labels[swarm.LabelService], "our spec won, not the racer's")
	assert.Equal(t, 3, count(f.CallLog(), "ServiceUpdate"), "racer's update, our stale attempt, our retry")
	assert.Equal(t, 3, count(f.CallLog(), "ServiceInspectWithRaw"), "ours before each attempt, plus the racer's")
}

func TestDeploy_PullPullsEveryImage(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "gitea", giteaCompose)
	require.NoError(t, d.Pull(ctx, "gitea"))
	assert.Equal(t, []string{"ImagePull gitea/gitea:1"}, f.CallLog())
}

func TestDeploy_UpRefusesAliasCollision(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	// another service whose container is also called "web"
	writeService(t, root, "other", strings.Replace(strings.Replace(webCompose, "%s", "x", 1), "  web:\n", "  front:\n", 1))
	require.NoError(t, d.Up(ctx, "web"))

	err := d.Up(ctx, "other")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "web")
	assert.Contains(t, err.Error(), "other")
	assert.Empty(t, services(t, f, "other"))
}

func TestDeploy_BindMountedFilesBecomeConfigs(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "web", `
services:
  web:
    image: nginx:1
    volumes:
      - ./conf:/etc/nginx/conf.d:ro
`)
	conf := filepath.Join(root, "services", "web", "conf")
	require.NoError(t, os.MkdirAll(filepath.Join(conf, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(conf, "a.conf"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(conf, "sub", "b.conf"), []byte("b"), 0o644))

	require.NoError(t, d.Up(ctx, "web"))
	refs := services(t, f, "web")[0].Spec.TaskTemplate.ContainerSpec.Configs
	var targets []string
	for _, r := range refs {
		assert.NotEmpty(t, r.ConfigID)
		targets = append(targets, r.File.Name)
	}
	slices.Sort(targets)
	assert.Equal(t, []string{"/etc/nginx/conf.d/a.conf", "/etc/nginx/conf.d/sub/b.conf"}, targets)
	cfgs, err := f.ConfigList(ctx, dswarm.ConfigListOptions{
		Filters: filters.NewArgs(filters.Arg("label", swarm.LabelService+"=web")),
	})
	require.NoError(t, err)
	assert.Len(t, cfgs, 2)
}

// R16: values BuildEnv took from the secrets manager must never reach a spec.
func TestDeploy_UpRefusesSecretInSpec(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	sm := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	require.NoError(t, sm.Set("web", "TOKEN", "s3cr3t-token"))
	d.SM = sm
	writeService(t, root, "web", `
services:
  web:
    image: nginx:1
    command: ["serve", "--token", "${TOKEN}"]
`)
	require.NoError(t, os.WriteFile(filepath.Join(root, "services", "web", "config.yaml"),
		[]byte("secrets:\n  TOKEN:\n    required: true\n"), 0o644))

	err := d.Up(ctx, "web")
	require.ErrorIs(t, err, swarm.ErrSecretInSpec)
	assert.NotContains(t, err.Error(), "s3cr3t-token")
	assert.Empty(t, services(t, f, "web"))
}

func TestDeploy_Status(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	f.SetNodes([]dswarm.Node{node("mini1", "core", dswarm.NodeStateReady, dswarm.NodeAvailabilityActive)})
	writeService(t, root, "web", strings.Replace(webCompose, "%s", "bar", 1))
	require.NoError(t, d.Up(ctx, "web"))
	id := services(t, f, "web")[0].ID
	f.AddTask(dswarm.Task{ID: "t0", ServiceID: id, NodeID: "mini1", DesiredState: dswarm.TaskStateShutdown,
		Status: dswarm.TaskStatus{State: dswarm.TaskStateFailed}})
	f.AddTask(dswarm.Task{ID: "t1", ServiceID: id, NodeID: "mini1", DesiredState: dswarm.TaskStateRunning,
		Status: dswarm.TaskStatus{State: dswarm.TaskStateRunning}})

	st, err := d.Status(ctx, "web")
	require.NoError(t, err)
	assert.Equal(t, []swarm.TaskStatus{{Service: "web_web", Node: "mini1", State: "running"}}, st)
}
