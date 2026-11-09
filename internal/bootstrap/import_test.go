package bootstrap

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/importer"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

const (
	sentGiteaDB = "giteadb-SENTINEL-pw"
	sentRedis   = "redis-SENTINEL-pw"
	sentGrafana = "grafana-SENTINEL-pw"
)

func writeOld(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(c), 0o644))
	}
}

// newImportInit is an init --import of a small Compose-era dir whose secrets
// are all in the keyring.
func newImportInit(t *testing.T) (*Init, *initHarness) {
	t.Helper()
	in, h := newInit(t, testIP)
	old := t.TempDir()
	writeOld(t, old, map[string]string{
		"config.yaml": "vars:\n  DOMAIN:\n    value: example.com\n  HOME_SUBDOMAIN:\n    value: home\n" +
			"secrets:\n  TS_AUTHKEY:\n    required: true\n",
		"state/desired.yaml":                "services:\n  gitea: running\n",
		"services/gitea/config.yaml":        "databases:\n  - postgres:\n      database: gitea\n      env:\n        password: GI_DB_PASS\nports:\n  - 3000\n  - 2222:2222\n",
		"services/gitea/docker-compose.yml": "services:\n  gitea:\n    image: gitea/gitea\n    environment:\n      P: ${GI_DB_PASS}\n",
		"services/gitea/exposure.yaml":      "layers: [ts]\n",
		"services/redis/config.yaml":        "secrets:\n  REDIS_PASSWORD:\n    required: true\n    generate: password\n",
		"services/redis/docker-compose.yml": "services:\n  redis:\n    image: redis\n    environment:\n      P: ${REDIS_PASSWORD:?unset}\n",
		"services/grafana/config.yaml":      "secrets:\n  GRAFANA_ADMIN_PASSWORD:\n    required: true\n  GF_OTHER:\n    required: true\n",
		"services/grafana/docker-compose.yml": "services:\n  grafana:\n    image: grafana/grafana\n" +
			"    volumes:\n      - ./provisioning:/etc/grafana/provisioning:ro\n",
		"services/grafana/provisioning/ds.yaml": "apiVersion: 1\n",
	})
	require.NoError(t, h.kr.Set("", config.DBPasswordKey("gitea"), sentGiteaDB))
	require.NoError(t, h.kr.Set("redis", "REDIS_PASSWORD", sentRedis))
	require.NoError(t, h.kr.Set("grafana", "GRAFANA_ADMIN_PASSWORD", sentGrafana))
	in.Import = old
	in.TempDir = t.TempDir()
	return in, h
}

// tarContent maps each entry of a tar stream to its content.
func tarContent(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		c, err := io.ReadAll(tr)
		require.NoError(t, err)
		out[h.Name] = string(c)
	}
}

func TestInit_Import(t *testing.T) {
	in, h := newImportInit(t)
	require.NoError(t, in.Do(context.Background()))

	copies := h.fake.Copies()
	require.Len(t, copies, 1)
	assert.Equal(t, AgentContainer, copies[0].Container)
	assert.Equal(t, agentstate.Root, copies[0].Path)
	files := tarContent(t, copies[0].Tar)
	var staging string
	for name := range files {
		staging, _, _ = strings.Cut(name, "/")
		break
	}
	assert.Regexp(t, `^import-[0-9a-f]{16}$`, staging)
	assert.Contains(t, files, staging+"/services/gitea/config.yaml")
	assert.Contains(t, files, staging+"/services/grafana/provisioning/ds.yaml")
	assert.Contains(t, files, staging+"/caddy/sites/gitea.conf")
	assert.Contains(t, files, staging+"/state/desired.yaml")

	execs := h.fake.Execs()
	require.Len(t, execs, 2, "the plain seed, then the import seed")
	assert.Equal(t, []string{"homelab", "agent-seed", "--root", agentstate.Root, "--import", staging},
		[]string(execs[1].Options.Cmd))
	var sec importer.Secrets
	require.NoError(t, json.Unmarshal(execs[1].Stdin, &sec))
	assert.Equal(t, sentGiteaDB, sec.Root[config.DBPasswordKey("gitea")])
	assert.Equal(t, sentRedis, sec.Services["redis"]["REDIS_PASSWORD"])
	assert.Equal(t, testTSKey, sec.Root["TS_AUTHKEY"])

	// Order: copy after the agent started, then the import seed, then one restart.
	log := strings.Join(h.fake.CallLog(), "\n")
	assert.Less(t, strings.Index(log, "ContainerStart"), strings.Index(log, "CopyToContainer"))
	assert.Less(t, strings.Index(log, "CopyToContainer"), strings.LastIndex(log, "ContainerExecCreate"))
	assert.Less(t, strings.LastIndex(log, "ContainerExecCreate"), strings.Index(log, "ContainerRestart"))
	assert.Equal(t, 1, h.fake.Created()[0].Restarts, "one restart reads both seeds")

	out := h.out.String()
	assert.Contains(t, out, "gitea: port 2222:2222 is tcp only now")
	assert.Contains(t, out, "svc:grafana:GF_OTHER")
	assert.Contains(t, out, "homelab setup grafana")
}

func TestImport_SecretsNeverInTarOrArgv(t *testing.T) {
	in, h := newImportInit(t)
	require.NoError(t, in.Do(context.Background()))
	sentinels := []string{sentGiteaDB, sentRedis, sentGrafana, testTSKey}

	for _, c := range h.fake.Copies() {
		for _, s := range sentinels {
			assert.NotContains(t, string(c.Tar), s, "a secret is in the tar stream")
		}
	}
	for _, e := range h.fake.Execs() {
		argv, err := json.Marshal(e.Options)
		require.NoError(t, err)
		for _, s := range sentinels {
			assert.NotContains(t, string(argv), s, "a secret is in an exec's argv/env")
		}
	}
	spec, err := json.Marshal(h.fake.Created())
	require.NoError(t, err)
	for _, s := range sentinels {
		assert.NotContains(t, string(spec), s)
		assert.NotContains(t, h.out.String(), s, "a secret is printed")
	}
	// The import exec's stdin is where they travel.
	execs := h.fake.Execs()
	assert.Contains(t, string(execs[len(execs)-1].Stdin), sentGrafana)
}

func TestImport_RefusesBridgeWithAttachedContainers(t *testing.T) {
	in, h := newImportInit(t)
	h.fake.AddNetwork(network.Summary{Name: "home-services", Driver: "bridge", Scope: "local",
		Containers: map[string]network.EndpointResource{"c1": {Name: "gitea"}}})
	err := in.Do(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "homelab down -a")
	assert.Contains(t, err.Error(), "gitea")
	assert.Empty(t, h.fake.Inits(), "refused before the swarm is created")
	assert.Empty(t, h.fake.Copies())
}

func TestImport_RemovesIdleComposeBridge(t *testing.T) {
	in, h := newImportInit(t)
	h.fake.AddNetwork(network.Summary{ID: "bridge1", Name: "home-services", Driver: "bridge", Scope: "local"})
	require.NoError(t, in.Do(context.Background()))
	log := strings.Join(h.fake.CallLog(), "\n")
	require.Contains(t, log, "NetworkRemove bridge1")
	assert.Less(t, strings.Index(log, "NetworkRemove bridge1"), strings.Index(log, "NetworkCreate home-services"))
}

func TestImport_TempDirRemovedOnError(t *testing.T) {
	// A seed fails in the container.
	in, h := newImportInit(t)
	h.fake.SetExecExitCode(1)
	require.Error(t, in.Do(context.Background()))
	left, err := os.ReadDir(in.TempDir)
	require.NoError(t, err)
	assert.Empty(t, left, "the temp tree is removed")

	// The old dir cannot be imported.
	in, _ = newImportInit(t)
	writeOld(t, in.Import, map[string]string{"services/Bad Name/config.yaml": "{}\n"})
	require.ErrorContains(t, in.Do(context.Background()), "Bad Name")
	left, err = os.ReadDir(in.TempDir)
	require.NoError(t, err)
	assert.Empty(t, left)

	// And on success.
	in, _ = newImportInit(t)
	require.NoError(t, in.Do(context.Background()))
	left, err = os.ReadDir(in.TempDir)
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestImport_StreamsTar(t *testing.T) {
	in, h := newImportInit(t)
	require.NoError(t, in.Do(context.Background()))
	copies := h.fake.Copies()
	require.Len(t, copies, 1)
	assert.Equal(t, "*io.PipeReader", copies[0].Reader, "the tar is streamed, not built in memory")
}

const composeWorkingDir = "com.docker.compose.project.working_dir"

// R42: any container of the Compose-era stack, stopped ones included, is
// refused before anything changes; other projects' containers are not ours.
func TestImport_RefusesComposeContainers(t *testing.T) {
	in, h := newImportInit(t)
	h.fake.SetContainers([]container.Summary{
		{ID: "pg", Names: []string{"/homelab-postgres"}, State: "exited",
			Labels: map[string]string{composeWorkingDir: filepath.Join(in.Import, "services", "postgres")}},
		{ID: "ts", Names: []string{"/tailscale"}, State: "exited",
			Labels: map[string]string{composeWorkingDir: filepath.Join(in.Import, "core")}},
		{ID: "tv", Names: []string{"/tv-kiosk"}, State: "running",
			Labels: map[string]string{composeWorkingDir: "/home/groot/tv-kiosk"}},
	})
	err := in.Do(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "homelab-postgres")
	assert.Contains(t, err.Error(), "tailscale")
	assert.NotContains(t, err.Error(), "tv-kiosk")
	assert.Contains(t, err.Error(), "homelab down -a")
	assert.Contains(t, err.Error(), "docker compose -f "+filepath.Join(in.Import, "core", "docker-compose.yml")+" down")
	assert.Empty(t, h.fake.Inits(), "refused before the swarm is created")
}

func TestImport_IgnoresOtherProjectsContainers(t *testing.T) {
	in, h := newImportInit(t)
	h.fake.SetContainers([]container.Summary{
		{ID: "tv", Names: []string{"/tv-kiosk"}, State: "running",
			Labels: map[string]string{composeWorkingDir: "/home/groot/tv-kiosk"}},
		// A sibling dir whose name merely starts with the import dir's.
		{ID: "sib", Names: []string{"/sibling"}, State: "exited",
			Labels: map[string]string{composeWorkingDir: in.Import + "-old/core"}},
		{ID: "plain", Names: []string{"/plain"}, State: "running"},
	})
	require.NoError(t, in.Do(context.Background()))
}

// A failure after the tree went in but before the agent restarted takes the
// agent container and its state volume away (never another volume), so a
// retry is not refused for "already has services".
func TestImport_LateFailureRemovesImportedState(t *testing.T) {
	in, h := newImportInit(t)
	h.fake.FailRestart(errors.New("restart failed"))
	err := in.Do(context.Background())
	require.ErrorContains(t, err, "restart failed")
	log := h.fake.CallLog()
	assert.Contains(t, log, "ContainerRemove "+AgentContainer)
	assert.Contains(t, log, "VolumeRemove "+StateVolume)
	for _, c := range log {
		if strings.HasPrefix(c, "VolumeRemove ") {
			assert.Equal(t, "VolumeRemove "+StateVolume, c, "only the agent's own state volume")
		}
	}
	assert.Equal(t, 1, h.fake.Leaves(), "and the swarm is left")
	assert.Contains(t, h.out.String(), "removed the agent and its state volume")
}

// Once the agent has restarted on the imported state it is kept: a later
// failure (saving the client config) does not delete anything.
func TestImport_FailureAfterRestartKeepsState(t *testing.T) {
	in, h := newImportInit(t)
	in.SaveClient = func(string) error { return errors.New("disk full") }
	require.ErrorContains(t, in.Do(context.Background()), "disk full")
	for _, c := range h.fake.CallLog() {
		assert.NotContains(t, c, "VolumeRemove")
		assert.NotContains(t, c, "ContainerRemove")
	}
}

// I-1: a state volume an earlier attempt left would be reused silently (vault
// included) and could be deleted by the rollback: refused up front.
func TestImport_RefusesLeftoverStateVolume(t *testing.T) {
	in, h := newImportInit(t)
	h.fake.SetVolumes(&volume.Volume{Name: StateVolume})
	err := in.Do(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a homelab-agent volume from an earlier attempt exists; if it holds nothing "+
		"you need, remove it with `docker volume rm homelab-agent` (data volumes are untouched)")
	assert.Empty(t, h.fake.Inits(), "no swarm call")
	assert.Empty(t, h.fake.Created())
	assert.Empty(t, h.prompts, "refused before any prompt")
}

// The rollback only removes a state volume this run created.
func TestImport_RollbackKeepsPreexistingVolume(t *testing.T) {
	f := swarmtest.New()
	f.SetImage("img", image.InspectResponse{})
	f.SetVolumes(&volume.Volume{Name: StateVolume})
	require.NoError(t, StartAgent(context.Background(), f, "img", true))
	var out bytes.Buffer
	removeAgentAndState(context.Background(), f, &out, false)
	log := f.CallLog()
	assert.Contains(t, log, "ContainerRemove "+AgentContainer)
	for _, c := range log {
		assert.NotContains(t, c, "VolumeRemove")
	}
	assert.Contains(t, out.String(), "predates this run and is kept")

	// A plain init over an existing volume never removes it on failure.
	in, h := newInit(t, testIP)
	h.fake.SetVolumes(&volume.Volume{Name: StateVolume})
	h.fake.FailRestart(errors.New("restart failed"))
	require.Error(t, in.Do(context.Background()))
	for _, c := range h.fake.CallLog() {
		assert.NotContains(t, c, "VolumeRemove")
	}
}

// M-2: a copy that returns nil after reading only part of the stream is a
// truncated tree: the import seed must not run on it.
func TestImport_RefusesTruncatedCopy(t *testing.T) {
	in, h := newImportInit(t)
	h.fake.SetCopyReadLimit(512)
	err := in.Do(context.Background())
	require.ErrorContains(t, err, "incomplete")
	for _, e := range h.fake.Execs() {
		assert.NotContains(t, e.Options.Cmd, "--import", "agent-seed --import never sees a truncated tree")
	}
}

// M-1: the import dir and the compose label may each be the symlinked
// spelling of the other.
func TestImport_ComposeContainersThroughSymlinks(t *testing.T) {
	for _, dir := range []string{"import via link", "label via link"} {
		t.Run(dir, func(t *testing.T) {
			in, h := newImportInit(t)
			link := filepath.Join(t.TempDir(), "link")
			require.NoError(t, os.Symlink(in.Import, link))
			wd := filepath.Join(in.Import, "services", "gitea")
			if dir == "import via link" {
				in.Import = link
			} else {
				wd = filepath.Join(link, "services", "gitea")
			}
			h.fake.SetContainers([]container.Summary{{ID: "g", Names: []string{"/gitea"}, State: "exited",
				Labels: map[string]string{composeWorkingDir: wd}}})
			err := in.Do(context.Background())
			require.ErrorContains(t, err, "gitea")
			assert.Empty(t, h.fake.Inits())
		})
	}
}
