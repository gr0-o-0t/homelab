package swarm_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
)

// composeVar matches ${VAR}, ${VAR:-default}, ${VAR:?msg} and friends.
var composeVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?[-?+])?([^}]*)\}`)

// dummyEnv gives every ${VAR} in a compose file a value: its default when it
// has one (ports and images parse), "1" otherwise.
func dummyEnv(t *testing.T, composeFile string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(composeFile)
	require.NoError(t, err)
	env := map[string]string{}
	for _, m := range composeVar.FindAllStringSubmatch(string(data), -1) {
		v := "1"
		if m[2] == "-" || m[2] == ":-" {
			v = m[3]
		}
		if _, seen := env[m[1]]; !seen || v != "1" {
			env[m[1]] = v
		}
	}
	return env
}

func translate(t *testing.T, name string) swarm.Stack {
	t.Helper()
	dir := filepath.Join("testdata", name)
	st, err := swarm.Translate(dir, dummyEnv(t, filepath.Join(dir, "docker-compose.yml")), nil, nil, false)
	require.NoError(t, err)
	return st
}

func specByName(t *testing.T, st swarm.Stack, name string) dswarm.ServiceSpec {
	t.Helper()
	for _, s := range st.Services {
		if s.Name == name {
			return s
		}
	}
	require.Failf(t, "no service spec", "%q in %d specs", name, len(st.Services))
	return dswarm.ServiceSpec{}
}

func attachment(s dswarm.ServiceSpec, target string) *dswarm.NetworkAttachmentConfig {
	for i := range s.TaskTemplate.Networks {
		if s.TaskTemplate.Networks[i].Target == target {
			return &s.TaskTemplate.Networks[i]
		}
	}
	return nil
}

func mountAt(s dswarm.ServiceSpec, target string) *mount.Mount {
	for i := range s.TaskTemplate.ContainerSpec.Mounts {
		if s.TaskTemplate.ContainerSpec.Mounts[i].Target == target {
			return &s.TaskTemplate.ContainerSpec.Mounts[i]
		}
	}
	return nil
}

func TestTranslate_GiteaAliasesAndVolume(t *testing.T) {
	st := translate(t, "gitea")
	assert.Equal(t, "gitea", st.Name)
	require.Len(t, st.Services, 1)
	s := specByName(t, st, "gitea_gitea")

	assert.Equal(t, map[string]string{
		"com.docker.stack.namespace": "gitea",
		"homelab.service":            "gitea",
		"homelab.role":               "core",
		"homelab.container_name":     "gitea",
	}, s.Labels)
	assert.Empty(t, s.TaskTemplate.Placement, "Task 8 applies constraints")

	cs := s.TaskTemplate.ContainerSpec
	// exec/logs find task containers by exact label, not by name prefix.
	assert.Equal(t, "gitea", cs.Labels["com.docker.stack.namespace"])
	assert.Equal(t, "gitea", cs.Labels["homelab.container_name"])
	assert.Equal(t, "gitea/gitea:latest-rootless", cs.Image)
	assert.Equal(t, "1000:1000", cs.User)
	assert.Nil(t, cs.Env, "env goes through a secret (Task 7), never the spec")

	a := attachment(s, "home-services")
	require.NotNil(t, a)
	assert.Equal(t, []string{"gitea"}, a.Aliases, "compose key and container_name, deduplicated")

	m := mountAt(s, "/var/lib/gitea")
	require.NotNil(t, m)
	assert.Equal(t, mount.TypeVolume, m.Type)
	assert.Equal(t, "gitea_data", m.Source)
	assert.ElementsMatch(t, []string{"gitea_data", "gitea_config"}, st.Volumes)

	require.NotNil(t, s.TaskTemplate.RestartPolicy)
	assert.Equal(t, dswarm.RestartPolicyConditionAny, s.TaskTemplate.RestartPolicy.Condition)

	hc := cs.Healthcheck
	require.NotNil(t, hc)
	assert.Equal(t, []string{"CMD-SHELL", "wget -q --spider http://localhost:3000 2>/dev/null || exit 1"}, hc.Test)
	assert.Equal(t, 30*time.Second, hc.Interval)
	assert.Equal(t, 10*time.Second, hc.Timeout)
	assert.Equal(t, 3, hc.Retries)
	assert.Equal(t, 30*time.Second, hc.StartPeriod)
	assert.Empty(t, st.Networks, "home-services is external; the deployer ensures it")
}

func TestTranslate_ImmichContainerNameAlias(t *testing.T) {
	st := translate(t, "immich")
	require.Len(t, st.Services, 2)
	s := specByName(t, st, "immich_immich-server")
	assert.Equal(t, "ghcr.io/immich-app/immich-server:release", s.TaskTemplate.ContainerSpec.Image)
	a := attachment(s, "home-services")
	require.NotNil(t, a)
	assert.Equal(t, []string{"immich-server", "immich"}, a.Aliases)
	for _, s := range st.Services {
		assert.Nil(t, s.TaskTemplate.ContainerSpec.Env, s.Name)
	}
}

func TestTranslate_InternalNetwork(t *testing.T) {
	st := translate(t, "internalnet")
	require.Len(t, st.Networks, 1)
	n := st.Networks[0]
	assert.Equal(t, "internalnet_internal", n.Name)
	assert.True(t, n.Internal)
	assert.Equal(t, "overlay", n.Driver)
	assert.Equal(t, map[string]string{"com.docker.network.driver.mtu": "1230"}, n.Options)
	assert.NotContains(t, n.Options, "encrypted")
	assert.Equal(t, "internalnet", n.Labels["com.docker.stack.namespace"])
	assert.Equal(t, "internalnet", n.Labels["homelab.service"])

	web := specByName(t, st, "internalnet_web")
	hs := attachment(web, "home-services")
	require.NotNil(t, hs)
	assert.Equal(t, []string{"web", "internalnet-web", "web-extra"}, hs.Aliases)
	assert.NotNil(t, attachment(web, "internalnet_internal"))

	worker := specByName(t, st, "internalnet_worker")
	assert.Nil(t, attachment(worker, "home-services"), "only containers compose puts on home-services join it")
	assert.NotNil(t, attachment(worker, "internalnet_internal"))
}

func TestTranslate_SeveralStackNetworksKeepTheirKeys(t *testing.T) {
	st := translate(t, "multinet")
	var names []string
	for _, n := range st.Networks {
		names = append(names, n.Name)
		assert.Equal(t, "overlay", n.Driver)
		assert.Equal(t, "1230", n.Options["com.docker.network.driver.mtu"])
	}
	assert.Equal(t, []string{"multinet_back", "multinet_default", "multinet_front"}, names)
	for _, n := range st.Networks {
		assert.Equal(t, n.Name != "multinet_default", n.Internal, n.Name)
	}
	assert.NotNil(t, attachment(specByName(t, st, "multinet_b"), "multinet_default"))
}

func TestTranslate_ContainerFields(t *testing.T) {
	st := translate(t, "internalnet")
	web := specByName(t, st, "internalnet_web")
	cs := web.TaskTemplate.ContainerSpec
	assert.Equal(t, "example/web:1", cs.Image)
	assert.Equal(t, []string{"/bin/run"}, cs.Command, "entrypoint")
	assert.Equal(t, []string{"--port", "8080"}, cs.Args, "command")
	assert.Equal(t, []string{"NET_ADMIN"}, cs.CapabilityAdd)
	assert.Equal(t, "yes", cs.Labels["example.label"])
	assert.Equal(t, dswarm.RestartPolicyConditionAny, web.TaskTemplate.RestartPolicy.Condition)

	data := mountAt(web, "/data")
	require.NotNil(t, data)
	assert.Equal(t, "internalnet_data", data.Source, "no name: → <svc>_<key>")
	assert.Equal(t, "shared_cache", mountAt(web, "/cache").Source)

	worker := specByName(t, st, "internalnet_worker")
	rp := worker.TaskTemplate.RestartPolicy
	assert.Equal(t, dswarm.RestartPolicyConditionOnFailure, rp.Condition)
	require.NotNil(t, rp.MaxAttempts)
	assert.Equal(t, uint64(3), *rp.MaxAttempts)

	job := specByName(t, st, "internalnet_job")
	assert.Equal(t, dswarm.RestartPolicyConditionNone, job.TaskTemplate.RestartPolicy.Condition)
	tmp := mountAt(job, "/scratch")
	require.NotNil(t, tmp)
	assert.Equal(t, mount.TypeTmpfs, tmp.Type)
}

func TestTranslate_RoleLabelFollowsPlacement(t *testing.T) {
	dir := filepath.Join("testdata", "internalnet")
	cfg := &config.Config{Placement: map[string]string{"job": "any"}}
	st, err := swarm.Translate(dir, dummyEnv(t, filepath.Join(dir, "docker-compose.yml")), nil, cfg, false)
	require.NoError(t, err)
	assert.Equal(t, "core", specByName(t, st, "internalnet_web").Labels["homelab.role"], "named volume")
	assert.Equal(t, "worker", specByName(t, st, "internalnet_worker").Labels["homelab.role"])
	assert.Equal(t, "any", specByName(t, st, "internalnet_job").Labels["homelab.role"])

	st, err = swarm.Translate(dir, dummyEnv(t, filepath.Join(dir, "docker-compose.yml")), nil, cfg, true)
	require.NoError(t, err)
	assert.Equal(t, "core", specByName(t, st, "internalnet_job").Labels["homelab.role"], "db provider")
}

func TestTranslate_BindUnderServiceDirBecomesConfig(t *testing.T) {
	st := translate(t, "glance")
	require.Len(t, st.Configs, 1)
	assert.Equal(t, swarm.FileConfig{
		Service: "glance_glance",
		Target:  "/app/config",
		Source:  "config",
		Mode:    0o444,
	}, st.Configs[0])
	s := specByName(t, st, "glance_glance")
	assert.Nil(t, mountAt(s, "/app/config"), "a config, not a bind")
	assert.Empty(t, st.Volumes)
}

func TestTranslate_HostBindStays(t *testing.T) {
	st := translate(t, "gitea")
	m := mountAt(specByName(t, st, "gitea_gitea"), "/etc/localtime")
	require.NotNil(t, m)
	assert.Equal(t, mount.TypeBind, m.Type)
	assert.Equal(t, "/etc/localtime", m.Source)
	assert.True(t, m.ReadOnly)
	assert.Empty(t, st.Configs)
}

func TestTranslate_PrivilegedUnsupported(t *testing.T) {
	_, err := swarm.Translate(filepath.Join("testdata", "privileged"), nil, nil, nil, false)
	require.Error(t, err)
	assert.True(t, errors.Is(err, swarm.ErrUnsupported))
	assert.Contains(t, err.Error(), "privileged")
	assert.Contains(t, err.Error(), "miner")
}

func TestTranslate_DevicesUnsupported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gpu")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(
		"services:\n  render:\n    image: example/render:1\n    devices:\n      - /dev/dri:/dev/dri\n"), 0o644))
	_, err := swarm.Translate(dir, nil, nil, nil, false)
	require.ErrorIs(t, err, swarm.ErrUnsupported)
	assert.Contains(t, err.Error(), "gpu/render")
}

// secretDBKeys are the databases: env mappings that carry credentials.
var secretDBKeys = []string{"password", "dsn", "secret_key", "access_key"}

// seedSecrets gives every secret-ish variable a distinct sentinel value in
// env and returns those values: the service's declared secrets, names
// secrets.IsSecret flags, and the env names its databases: credentials bind.
func seedSecrets(t *testing.T, env map[string]string, cfg *config.Config) []string {
	t.Helper()
	names := map[string]bool{}
	for name := range env {
		if secrets.IsSecret(name) {
			names[name] = true
		}
	}
	if cfg != nil {
		for name := range cfg.Secrets {
			names[name] = true
		}
		dbs, err := cfg.ServiceDatabases()
		require.NoError(t, err)
		for _, d := range dbs {
			for _, k := range secretDBKeys {
				if n := d.Env[k]; n != "" {
					names[n] = true
				}
			}
		}
	}
	var values []string
	for name := range names {
		v := "s3ntinel-" + name + "-x"
		env[name] = v
		values = append(values, v)
	}
	return values
}

func TestTranslate_SecretInCommandRejected(t *testing.T) {
	dir := filepath.Join("testdata", "secretcmd")
	env := map[string]string{"APP_TOKEN": "hunter2-very-secret"}
	_, err := swarm.Translate(dir, env, []string{"hunter2-very-secret"}, nil, false)
	require.ErrorIs(t, err, swarm.ErrSecretInSpec)
	assert.Contains(t, err.Error(), "secretcmd/app")
	assert.NotContains(t, err.Error(), "hunter2")

	_, err = swarm.Translate(dir, env, []string{"abc"}, nil, false)
	assert.NoError(t, err, "values under 4 bytes are not checked")
}

func TestTranslate_GiteaEnvironmentResolved(t *testing.T) {
	dir := filepath.Join("testdata", "gitea")
	env := dummyEnv(t, filepath.Join(dir, "docker-compose.yml"))
	env["GI_DB_HOST"] = "homelab-postgres"
	env["GI_DB_PORT"] = "5432"
	env["GI_DB_PASS"] = "pg-pass-value"
	st, err := swarm.Translate(dir, env, []string{"pg-pass-value"}, nil, false)
	require.NoError(t, err)

	e := st.Env["gitea_gitea"]
	require.NotNil(t, e)
	assert.Equal(t, "homelab-postgres:5432", e["GITEA__database__HOST"])
	assert.Equal(t, "pg-pass-value", e["GITEA__database__PASSWD"])
	assert.Equal(t, "2222", e["GITEA__server__SSH_PORT"])

	data, err := json.Marshal(st.Services)
	require.NoError(t, err)
	for _, v := range []string{"homelab-postgres:5432", "pg-pass-value", "GITEA__database"} {
		assert.NotContains(t, string(data), v)
	}
}

func TestTranslate_EnvironmentWithoutValueTakesEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bare")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(
		"services:\n  app:\n    image: example/app:1\n    environment:\n      - PRESENT\n      - ABSENT\n"), 0o644))
	st, err := swarm.Translate(dir, map[string]string{"PRESENT": "yes"}, nil, nil, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"PRESENT": "yes"}, st.Env["bare_app"])
}

func TestTranslate_WritableBindUnderServiceDirUnsupported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rw")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(
		"services:\n  app:\n    image: example/app:1\n    volumes:\n      - ./state.json:/state.json\n"), 0o644))
	_, err := swarm.Translate(dir, nil, nil, nil, false)
	require.ErrorIs(t, err, swarm.ErrUnsupported)
	assert.Contains(t, err.Error(), "rw/app")
	assert.Contains(t, err.Error(), "state.json")
}

// Every catalog service but minero (privileged xmrig) must translate.
func TestTranslate_EveryCatalogServiceTranslates(t *testing.T) {
	root := filepath.Join("..", "..", "assets", "services")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		compose := filepath.Join(dir, "docker-compose.yml")
		if _, err := os.Stat(compose); err != nil {
			continue
		}
		n++
		t.Run(e.Name(), func(t *testing.T) {
			var cfg *config.Config
			if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
				cfg, err = config.Load(filepath.Join(dir, "config.yaml"))
				require.NoError(t, err)
			}
			_, provider := config.IsSharedDBService(e.Name())
			env := dummyEnv(t, compose)
			sentinels := seedSecrets(t, env, cfg)
			st, err := swarm.Translate(dir, env, sentinels, cfg, provider)
			if e.Name() == "minero" {
				require.ErrorIs(t, err, swarm.ErrUnsupported)
				assert.Contains(t, err.Error(), "minero")
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, st.Services)
			for _, s := range st.Services {
				assert.Nil(t, s.TaskTemplate.ContainerSpec.Env, s.Name)
				assert.NotEmpty(t, s.TaskTemplate.ContainerSpec.Image, s.Name)
			}
		})
	}
	assert.Greater(t, n, 50)
}

// Every task container carries its stack and compose container_name as
// labels, so exec/logs select a stack's containers exactly.
func TestTranslate_ContainerLabelsNameStackAndContainer(t *testing.T) {
	st := translate(t, "immich")
	ml := specByName(t, st, "immich_immich-machine-learning")
	assert.Equal(t, "immich", ml.TaskTemplate.ContainerSpec.Labels["com.docker.stack.namespace"])
	assert.Equal(t, "immich-machine-learning", ml.TaskTemplate.ContainerSpec.Labels["homelab.container_name"])
	srv := specByName(t, st, "immich_immich-server")
	assert.Equal(t, "immich", srv.Labels["homelab.container_name"], "on the service too, for -c on the leader")

	for _, s := range translate(t, "multinet").Services {
		assert.Equal(t, "multinet", s.TaskTemplate.ContainerSpec.Labels["com.docker.stack.namespace"])
		_, has := s.TaskTemplate.ContainerSpec.Labels["homelab.container_name"]
		assert.False(t, has, "%s has no container_name", s.Name)
	}
}

// LoadProject names the stack after the directory and interpolates env, as
// Translate does: what `homelab config` and `images` show is what deploys.
func TestLoadProject_InterpolatesAndNamesStack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "web")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: nginx:${TAG:-latest}\n"), 0o600))
	p, err := swarm.LoadProject(dir, map[string]string{"TAG": "1.27"})
	require.NoError(t, err)
	assert.Equal(t, "web", p.Name)
	assert.Equal(t, "nginx:1.27", p.Services["app"].Image)
}

// pull_policy: never marks an image built locally, so deploy and update
// know no registry has it.
func TestTranslate_PullPolicyNeverLabelsLocal(t *testing.T) {
	cs := specByName(t, translate(t, "taash"), "taash_taash").TaskTemplate.ContainerSpec
	assert.Equal(t, "true", cs.Labels[swarm.LabelImageLocal])
	assert.True(t, swarm.IsLocalImage(cs))

	gs := specByName(t, translate(t, "glance"), "glance_glance").TaskTemplate.ContainerSpec
	_, has := gs.Labels[swarm.LabelImageLocal]
	assert.False(t, has)
	assert.False(t, swarm.IsLocalImage(gs))
}

func TestTranslate_TaashHardeningCarried(t *testing.T) {
	cs := specByName(t, translate(t, "taash"), "taash_taash").TaskTemplate.ContainerSpec
	assert.True(t, cs.ReadOnly)
	assert.Equal(t, "65532:65532", cs.User)
	assert.Equal(t, []string{"ALL"}, cs.CapabilityDrop)
	require.NotNil(t, cs.Privileges)
	assert.True(t, cs.Privileges.NoNewPrivileges)
}
