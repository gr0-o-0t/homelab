package swarm_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	assert.NotContains(t, f.CallLog(), "ImagePull nginx:1", "nginx:1 is on the engine: up uses it (R53)")

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
	assert.Equal(t, uint64(1), replicas(services(t, f, "web")[0]), "restart means running afterwards")
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
	_, err := d.Pull(ctx, "web")
	require.NoError(t, err)
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
}

func TestDeploy_PullPullsEveryImage(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "gitea", giteaCompose)
	skipped, err := d.Pull(ctx, "gitea")
	require.NoError(t, err)
	assert.Empty(t, skipped)
	assert.Equal(t, 1, count(f.CallLog(), "ImagePull gitea/gitea:1"))
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

// Down only changes the spec; a backup must not tar a volume while the old
// task's container is still shutting down.
func TestDeploy_WaitStoppedWaitsForTasksToStop(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "gitea", giteaCompose)
	require.NoError(t, d.Up(ctx, "gitea"))
	svc := services(t, f, "gitea")[0]
	task := func(state dswarm.TaskState) dswarm.Task {
		return dswarm.Task{ID: "t1", ServiceID: svc.ID, DesiredState: dswarm.TaskStateShutdown,
			Status: dswarm.TaskStatus{State: state}}
	}

	f.SetTasks([]dswarm.Task{task(dswarm.TaskStateRunning)})
	err := d.WaitStopped(ctx, "gitea", 50*time.Millisecond)
	assert.ErrorContains(t, err, "still running")

	f.SetTasks([]dswarm.Task{task(dswarm.TaskStateShutdown), {ID: "t0", ServiceID: svc.ID, Status: dswarm.TaskStatus{State: dswarm.TaskStateFailed}}})
	assert.NoError(t, d.WaitStopped(ctx, "gitea", time.Second))
}

// Scale changes only the counts: no pull, no registry query, no new spec
// from the (possibly edited) compose file.
func TestDeploy_ScaleChangesOnlyReplicas(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "gitea", giteaCompose)
	require.NoError(t, d.Up(ctx, "gitea"))
	before := services(t, f, "gitea")[0]
	name := before.Spec.Name
	writeService(t, root, "gitea", strings.ReplaceAll(giteaCompose, "gitea/gitea:1", "gitea/gitea:2"))
	calls := len(f.CallLog())

	reps, err := d.Replicas(ctx, "gitea")
	require.NoError(t, err)
	assert.Equal(t, map[string]uint64{name: 1}, reps)

	require.NoError(t, d.Scale(ctx, "gitea", map[string]uint64{name: 0}))
	got, err := d.Replicas(ctx, "gitea")
	require.NoError(t, err)
	assert.Equal(t, map[string]uint64{name: 0}, got)

	require.NoError(t, d.Scale(ctx, "gitea", reps))
	after := services(t, f, "gitea")[0]
	assert.Equal(t, uint64(1), replicas(after))
	assert.Equal(t, before.Spec.TaskTemplate.ContainerSpec.Image, after.Spec.TaskTemplate.ContainerSpec.Image,
		"the compose edit is not deployed as a side effect")
	for _, c := range f.CallLog()[calls:] {
		assert.NotContains(t, c, "ImagePull")
		assert.NotContains(t, c, "ImageInspect")
	}
}

// perNodeCompose is a hub with data plus an agent on every node.
const perNodeCompose = `
networks:
  home-services:
    name: home-services
    external: true
services:
  hub:
    image: gitea/gitea:1
    container_name: hub
    restart: always
    volumes:
      - hub-data:/data
    networks: [home-services]
  agent:
    image: nginx:1
    container_name: agent
    restart: always
    volumes:
      - /proc:/host/proc:ro
    networks: [home-services]
    deploy:
      mode: global
volumes:
  hub-data:
    name: mon_hub_data
`

func byName(t *testing.T, f *swarmtest.Fake, svc string) map[string]dswarm.Service {
	t.Helper()
	out := map[string]dswarm.Service{}
	for _, s := range services(t, f, svc) {
		out[s.Spec.Name] = s
	}
	return out
}

func TestDeploy_UpKeepsGlobalModeUnconstrained(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	f.SetNodes([]dswarm.Node{node("w1", "worker", dswarm.NodeStateReady, dswarm.NodeAvailabilityActive)})
	writeService(t, root, "mon", perNodeCompose)

	require.NoError(t, d.Up(ctx, "mon"))
	require.NoError(t, d.Up(ctx, "mon"), "a second up updates the global service in place")

	agent := byName(t, f, "mon")["mon_agent"]
	assert.NotNil(t, agent.Spec.Mode.Global)
	assert.Nil(t, agent.Spec.Mode.Replicated)
	assert.Nil(t, agent.Spec.TaskTemplate.Placement, "every node, workers and core alike")
	assert.Equal(t, uint64(1), replicas(byName(t, f, "mon")["mon_hub"]))
}

// Swarm can neither scale a global service to zero nor change its mode:
// down removes it (its volumes stay) and up creates it again.
func TestDeploy_DownRemovesGlobalAndUpRecreatesIt(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "mon", perNodeCompose)
	f.SetVolumes(&volume.Volume{Name: "mon_hub_data"})
	require.NoError(t, d.Up(ctx, "mon"))

	require.NoError(t, d.Down(ctx, "mon"))
	svcs := byName(t, f, "mon")
	assert.NotContains(t, svcs, "mon_agent")
	assert.Equal(t, uint64(0), replicas(svcs["mon_hub"]))
	assert.Equal(t, 0, count(f.CallLog(), "VolumeRemove"))

	require.NoError(t, d.Up(ctx, "mon"))
	assert.NotNil(t, byName(t, f, "mon")["mon_agent"].Spec.Mode.Global)
}

func TestDeploy_RestartAfterDownBringsGlobalBack(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "mon", perNodeCompose)
	require.NoError(t, d.Up(ctx, "mon"))
	require.NoError(t, d.Down(ctx, "mon"))

	require.NoError(t, d.Restart(ctx, "mon"))
	svcs := byName(t, f, "mon")
	assert.Contains(t, svcs, "mon_agent")
	assert.Equal(t, uint64(1), replicas(svcs["mon_hub"]))
}

// A backup stops the replicated containers and leaves the per-node agents
// running: they have no count to record, and their tasks never stop.
func TestDeploy_BackupLifecycleIgnoresGlobal(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "mon", perNodeCompose)
	require.NoError(t, d.Up(ctx, "mon"))

	reps, err := d.Replicas(ctx, "mon")
	require.NoError(t, err)
	assert.Equal(t, map[string]uint64{"mon_hub": 1}, reps)

	agent := byName(t, f, "mon")["mon_agent"]
	f.SetTasks([]dswarm.Task{{ID: "a1", ServiceID: agent.ID, DesiredState: dswarm.TaskStateRunning,
		Status: dswarm.TaskStatus{State: dswarm.TaskStateRunning}}})
	assert.NoError(t, d.WaitStopped(ctx, "mon", 50*time.Millisecond))
}

// taashCompose is a service whose image is built locally (pull_policy:
// never): no registry has it.
const taashCompose = `
networks:
  home-services:
    name: home-services
    external: true
services:
  taash:
    image: taash-server:current
    pull_policy: never
    container_name: taash
    environment:
      TAASH_PORT: "8080"
    networks: [home-services]
`

const taashRef = "taash-server:current"

// localImage is a locally built image as the engine reports it: no repo
// digest, and an identity made of its layers and creation time.
func localImage(created string, layers ...string) image.InspectResponse {
	return image.InspectResponse{
		ID:      "sha256:" + created,
		Created: created,
		RootFS:  image.RootFS{Type: "layers", Layers: layers},
		Config:  &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Entrypoint: []string{"/app/taash-server"}}},
	}
}

func taashContainer(t *testing.T, f *swarmtest.Fake) *dswarm.ContainerSpec {
	t.Helper()
	svcs := services(t, f, "taash")
	require.Len(t, svcs, 1)
	return svcs[0].Spec.TaskTemplate.ContainerSpec
}

func TestDeploy_LocalImageNeverPulled(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	img := localImage("2026-11-12T10:00:00Z", "sha256:l1")
	f.SetImage(taashRef, img)
	writeService(t, root, "taash", taashCompose)

	require.NoError(t, d.Up(ctx, "taash"))
	cs := taashContainer(t, f)
	assert.Equal(t, swarm.ImageIdentity(img), cs.Labels[swarm.LabelImageIdentity])
	assert.Equal(t, taashRef, cs.Image, "a local image is never pinned to a digest")

	require.NoError(t, d.Update(ctx, "taash"))
	cs = taashContainer(t, f)
	assert.Equal(t, swarm.ImageIdentity(img), cs.Labels[swarm.LabelImageIdentity])
	assert.Equal(t, taashRef, cs.Image)
	assert.Zero(t, count(f.CallLog(), "ImagePull"))
	assert.Equal(t, []bool{false, false}, f.RegistryQueries(), "no registry is asked about a local image")
}

func TestDeploy_LocalImageMissingFails(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "taash", taashCompose)

	for _, verb := range []func(context.Context, string) error{d.Up, d.Update} {
		err := verb(ctx, "taash")
		require.ErrorIs(t, err, swarm.ErrLocalImageMissing)
		assert.ErrorContains(t, err, "image taash-server:current is local: run `homelab up|update taash` from the PC that built it")
	}
	assert.Zero(t, count(f.CallLog(), "ImagePull"))
	assert.Zero(t, count(f.CallLog(), "ServiceCreate"))
	assert.Empty(t, services(t, f, "taash"))
}

func TestUpdate_LocalImageRollsOnNewIdentity(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	f.SetImage(taashRef, localImage("2026-11-12T10:00:00Z", "sha256:l1"))
	writeService(t, root, "taash", taashCompose)
	require.NoError(t, d.Up(ctx, "taash"))

	rebuilt := localImage("2026-11-12T11:00:00Z", "sha256:l1", "sha256:l2")
	f.SetImage(taashRef, rebuilt)
	require.NoError(t, d.Update(ctx, "taash"))
	cs := taashContainer(t, f)
	assert.Equal(t, swarm.ImageIdentity(rebuilt), cs.Labels[swarm.LabelImageIdentity],
		"the changed container label is what rolls the tasks")
	assert.Zero(t, count(f.CallLog(), "ImagePull"))
}

func TestUp_LocalImageKeepsDeployedIdentity(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	first := localImage("2026-11-12T10:00:00Z", "sha256:l1")
	f.SetImage(taashRef, first)
	writeService(t, root, "taash", taashCompose)
	require.NoError(t, d.Up(ctx, "taash"))

	f.SetImage(taashRef, localImage("2026-11-12T11:00:00Z", "sha256:l1", "sha256:l2"))
	require.NoError(t, d.Up(ctx, "taash"))
	assert.Equal(t, swarm.ImageIdentity(first), taashContainer(t, f).Labels[swarm.LabelImageIdentity],
		"up keeps the image the service was deployed with (R53)")
}

func TestPull_SkipsLocalImage(t *testing.T) {
	ctx := context.Background()
	d, f, root := newDeployer(t)
	writeService(t, root, "taash", taashCompose)
	skipped, err := d.Pull(ctx, "taash")
	require.NoError(t, err)
	assert.Equal(t, []string{taashRef}, skipped)
	assert.Empty(t, f.CallLog())
}
