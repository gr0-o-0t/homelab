package importer

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/secrets"
)

func TestMain(m *testing.M) {
	// Off the real Docker daemon: see cmd/main_test.go.
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-test-docker.sock")
	os.Exit(m.Run())
}

func put(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

const rootConfig = `vars:
  DOMAIN:
    value: example.com
    required: true
  HOME_SUBDOMAIN:
    value: home
    required: true
secrets:
  TS_AUTHKEY:
    required: true
  CLOUDFLARE_API_TOKEN:
    required: true
`

// gitea's config.yaml on minis: the legacy mapping form, a raw port on both
// protocols.
const giteaConfig = `vars:
  PUID:
    value: "1000"
    required: false
secrets: {}
databases:
  - postgres:
      database: gitea
      user: gitea
      env:
        password: GI_DB_PASS
ports:
  "22":
    port: 2222
    listen: 2222
    protocols: [tcp, udp]
  default:
    port: 3000
    protocols: [tcp, udp]
`

const giteaCompose = `networks:
  home-services:
    name: home-services
    external: true
services:
  gitea:
    image: gitea/gitea:latest-rootless
    container_name: gitea
    user: "${PUID:-1000}:${PGID:-1000}"
    environment:
      GITEA__database__PASSWD: ${GI_DB_PASS}
    volumes:
      - gitea-data:/var/lib/gitea
      - /etc/timezone:/etc/timezone:ro
    networks: [home-services]
volumes:
  gitea-data:
    name: gitea_data
`

const glanceCompose = `networks:
  home-services:
    name: home-services
    external: true
services:
  glance:
    image: glanceapp/glance:latest
    container_name: glance
    volumes:
      - ./config:/app/config:ro
    networks: [home-services]
`

const grafanaCompose = `services:
  grafana:
    image: grafana/grafana
    environment:
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_ADMIN_PASSWORD}
    volumes:
      - grafana-data:/var/lib/grafana
      - ./provisioning:/etc/grafana/provisioning:ro
      - ./grafana.ini:/etc/grafana/grafana.ini:ro
volumes:
  grafana-data:
    name: homelab_grafana_data
`

const immichConfig = `secrets: {}
databases:
  - postgres:
      database: immich
      user: immich
      env:
        password: DB_PASSWORD
  - redis:
      env:
        password: REDIS_PASSWORD
        db: REDIS_DBINDEX
ports:
  - 2283
`

const immichCompose = `services:
  immich-server:
    image: ghcr.io/immich-app/immich-server:release
    container_name: immich_server
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    volumes:
      - immich-upload:/usr/src/app/upload
volumes:
  immich-upload:
`

const redisConfig = `secrets:
  REDIS_PASSWORD:
    required: true
    generate: password
`

const redisCompose = `services:
  redis:
    image: redis:8-alpine
    container_name: homelab-redis
    environment:
      REDIS_PASSWORD: ${REDIS_PASSWORD:?REDIS_PASSWORD is not set}
`

const postgresConfig = `secrets:
  POSTGRES_ROOT_PASSWORD:
    required: true
`

const postgresCompose = `services:
  postgres:
    image: ghcr.io/tensorchord/vchord-postgres:pg18-v1.1.1
    container_name: homelab-postgres
    environment:
      POSTGRES_PASSWORD: ${POSTGRES_ROOT_PASSWORD}
    volumes:
      - postgres-data:/var/lib/postgresql
volumes:
  postgres-data:
`

const vaultwardenConfig = `secrets: {}
ports:
  - vault:80
`

const vaultwardenCompose = `services:
  vaultwarden:
    image: vaultwarden/server
    container_name: vaultwarden
`

// Sentinel secret values: none may appear in the tree or the tar.
const (
	sentTS       = "tskey-SENTINEL-ts"
	sentCF       = "cf-SENTINEL-token"
	sentGiteaDB  = "giteadb-SENTINEL-pw"
	sentImmichDB = "immichdb-SENTINEL-pw"
	sentRedis    = "redis-SENTINEL-pw"
	sentPG       = "pgroot-SENTINEL-pw"
	sentStray    = "stray-SENTINEL-undeclared"
)

var sentinels = []string{sentTS, sentCF, sentGiteaDB, sentImmichDB, sentRedis, sentPG, sentStray}

// oldDir is ~/.config/homelab on minis, as of the cutover.
func oldDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	put(t, filepath.Join(d, "config.yaml"), rootConfig)
	put(t, filepath.Join(d, "redis-dbs.yaml"), "immich: 1\n")
	put(t, filepath.Join(d, "state", "desired.yaml"), "core: running\nservices:\n  gitea: running\n  immich: running\n  redis: running\n")
	// Compose-era leftovers that must not travel.
	put(t, filepath.Join(d, "core", "docker-compose.yml"), "services: {}\n")
	put(t, filepath.Join(d, "caddy", "Caddyfile"), "{}\n")
	put(t, filepath.Join(d, "tor", "torrc"), "x\n")
	put(t, filepath.Join(d, "i2p", "tunnels.conf"), "x\n")
	put(t, filepath.Join(d, "yggdrasil", "yggdrasil.conf"), "x\n")

	s := func(svc, file, content string) { put(t, filepath.Join(d, "services", svc, file), content) }
	s("gitea", "config.yaml", giteaConfig)
	s("gitea", "docker-compose.yml", giteaCompose)
	s("gitea", "exposure.yaml", "layers: [ts]\n")
	s("gitea", "notes.txt", "junk\n")

	s("glance", "config.yaml", "ports:\n  - 8080\n")
	s("glance", "docker-compose.yml", glanceCompose)
	s("glance", "exposure.yaml", "layers: [ts]\n")
	s("glance", "config/glance.yml", "pages: []\n")
	s("glance", "config/sub/theme.css", "body{}\n")

	s("grafana", "config.yaml", "secrets:\n  GRAFANA_ADMIN_PASSWORD:\n    required: true\nports:\n  - 3000\n")
	s("grafana", "docker-compose.yml", grafanaCompose)
	s("grafana", "provisioning/datasources/ds.yaml", "apiVersion: 1\n")
	s("grafana", "grafana.ini", "[server]\n")
	s("grafana", "caddy.routes.conf", "reverse_proxy grafana:3000\n")

	s("immich", "config.yaml", immichConfig)
	s("immich", "docker-compose.yml", immichCompose)
	s("immich", "exposure.yaml", "layers: [ts, tor]\n")

	s("redis", "config.yaml", redisConfig)
	s("redis", "docker-compose.yml", redisCompose)
	s("postgres", "config.yaml", postgresConfig)
	s("postgres", "docker-compose.yml", postgresCompose)

	// vaultwarden still routed by a legacy per-layer file, no exposure.yaml.
	s("vaultwarden", "config.yaml", vaultwardenConfig)
	s("vaultwarden", "docker-compose.yml", vaultwardenCompose)
	put(t, filepath.Join(d, "caddy", "conf.d", "vaultwarden-vault.conf"),
		"vault.{$HOME_SUBDOMAIN}.{$DOMAIN} {\n\timport wildcard_tls\n\treverse_proxy vaultwarden:80\n}\n")
	return d
}

// minisKeyring holds every secret but grafana's.
func minisKeyring(t *testing.T) *secrets.Manager {
	t.Helper()
	kr := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	for _, s := range []struct{ ns, k, v string }{
		{"", "TS_AUTHKEY", sentTS},
		{"", "CLOUDFLARE_API_TOKEN", sentCF},
		{"", config.DBPasswordKey("gitea"), sentGiteaDB},
		{"", config.DBPasswordKey("immich"), sentImmichDB},
		{"redis", "REDIS_PASSWORD", sentRedis},
		{"postgres", "POSTGRES_ROOT_PASSWORD", sentPG},
		{"gitea", "STRAY", sentStray}, // not declared anywhere: not imported
	} {
		require.NoError(t, kr.Set(s.ns, s.k, s.v))
	}
	return kr
}

func build(t *testing.T) (dst string, rep Report, sec Secrets) {
	t.Helper()
	dst = t.TempDir()
	rep, sec, err := Build(oldDir(t), dst, minisKeyring(t))
	require.NoError(t, err)
	return dst, rep, sec
}

func TestImport_CopiesServicesAndFiles(t *testing.T) {
	dst, rep, _ := build(t)
	assert.Equal(t, []string{"gitea", "glance", "grafana", "immich", "postgres", "redis", "vaultwarden"}, rep.Services)

	for _, f := range []string{
		"config.yaml", "redis-dbs.yaml",
		"services/gitea/config.yaml", "services/gitea/docker-compose.yml", "services/gitea/exposure.yaml",
		"services/glance/config/glance.yml", "services/glance/config/sub/theme.css",
		"services/grafana/provisioning/datasources/ds.yaml", "services/grafana/grafana.ini",
		"services/grafana/caddy.routes.conf",
		"services/immich/docker-compose.yml", "services/redis/config.yaml",
	} {
		assert.FileExists(t, filepath.Join(dst, f))
	}
	d, exists, err := desired.LoadDesired(dst)
	require.NoError(t, err)
	require.True(t, exists, "desired.yaml is where internal/desired reads it")
	assert.True(t, d.ServiceRunning("immich"))

	for _, gone := range []string{"core", "tor", "i2p", "yggdrasil", "caddy/Caddyfile", "caddy/conf.d",
		"services/gitea/notes.txt"} {
		assert.NoFileExists(t, filepath.Join(dst, gone))
		assert.NoDirExists(t, filepath.Join(dst, gone))
	}
	// Modes: files 0600, dirs 0700.
	fi, err := os.Stat(filepath.Join(dst, "services/glance/config/glance.yml"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	fi, err = os.Stat(filepath.Join(dst, "services/glance/config"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
}

func TestImport_RefusesInvalidServiceName(t *testing.T) {
	old := oldDir(t)
	put(t, filepath.Join(old, "services", "Bad Name", "config.yaml"), "{}\n")
	_, _, err := Build(old, t.TempDir(), minisKeyring(t))
	require.ErrorContains(t, err, "Bad Name")
}

func TestImport_SecretsNotInTree(t *testing.T) {
	dst, _, sec := build(t)
	var tarBuf bytes.Buffer
	require.NoError(t, Tar(&tarBuf, dst, "import-00"))
	require.NoError(t, filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
		require.NoError(t, err)
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p) //nolint:gosec // test tree
			require.NoError(t, err)
			for _, s := range sentinels {
				assert.NotContains(t, string(b), s, p)
			}
		}
		return nil
	}))
	for _, s := range sentinels {
		assert.NotContains(t, tarBuf.String(), s)
	}
	assert.Equal(t, sentTS, sec.Root["TS_AUTHKEY"])
}

func TestImport_MovesSecretsToVault(t *testing.T) {
	dst, _, sec := build(t)
	assert.NotContains(t, sec.Services["gitea"], "STRAY", "undeclared keyring entries are not imported")

	root := t.TempDir()
	staging := stage(t, dst, root)
	payload, err := json.Marshal(sec)
	require.NoError(t, err)
	stored, err := ApplySeed(root, staging, bytes.NewReader(payload))
	require.NoError(t, err)
	assert.Contains(t, stored, "root:TS_AUTHKEY")
	assert.Contains(t, stored, "svc:redis:REDIS_PASSWORD")
	for _, n := range stored {
		assert.NotContains(t, strings.Join(sentinels, " "), n)
	}

	p := agentstate.At(root)
	vault, err := secrets.OpenVault(p.VaultFile, p.KeyFile)
	require.NoError(t, err)
	for _, s := range []struct{ ns, k, v string }{
		{"", "TS_AUTHKEY", sentTS},
		{"", "CLOUDFLARE_API_TOKEN", sentCF},
		{"", config.DBPasswordKey("gitea"), sentGiteaDB},
		{"", config.DBPasswordKey("immich"), sentImmichDB},
		{"redis", "REDIS_PASSWORD", sentRedis},
		{"postgres", "POSTGRES_ROOT_PASSWORD", sentPG},
	} {
		got, err := vault.Get(s.ns, s.k)
		require.NoError(t, err)
		assert.Equal(t, s.v, got, s.ns+":"+s.k)
	}
	// The tree moved into the state dir; the staging dir is gone.
	assert.FileExists(t, filepath.Join(p.StateDir, "services/gitea/config.yaml"))
	assert.FileExists(t, filepath.Join(p.StateDir, "state/desired.yaml"))
	assert.NoDirExists(t, filepath.Join(root, staging))

	// A second import is refused: the state dir has services now.
	staging = stage(t, dst, root)
	_, err = ApplySeed(root, staging, bytes.NewReader(payload))
	require.ErrorContains(t, err, "already has services")
	assert.NoDirExists(t, filepath.Join(root, staging))
}

func TestAgentSeedImport_RejectsUndeclaredNames(t *testing.T) {
	dst, _, _ := build(t)
	for name, payload := range map[string]string{
		"root var":         `{"root":{"DOMAIN":"x"}}`,
		"undeclared svc":   `{"services":{"gitea":{"STRAY":"x"}}}`,
		"unknown service":  `{"services":{"nope":{"X":"x"}}}`,
		"db pw of non-db":  `{"root":{"DB_PASSWORD_GLANCE":"x"}}`,
		"redis pw outside": `{"services":{"immich":{"REDIS_PASSWORD":"x"}}}`,
		"garage key no s3": `{"services":{"gitea":{"GARAGE_ACCESS_KEY_ID":"x"}}}`,
		"unknown field":    `{"root":{},"extra":{}}`,
		"empty value":      `{"root":{"TS_AUTHKEY":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			staging := stage(t, dst, root)
			_, err := ApplySeed(root, staging, strings.NewReader(payload))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), `"x"`)
			p := agentstate.At(root)
			assert.NoFileExists(t, p.VaultFile, "nothing stored when any name is refused")
			assert.NoDirExists(t, filepath.Join(p.StateDir, "services"))
			assert.NoDirExists(t, filepath.Join(root, staging))
		})
	}
	// A bad staging name never reaches the filesystem.
	_, err := ApplySeed(t.TempDir(), "../etc", strings.NewReader(`{}`))
	require.Error(t, err)
}

func TestImport_ReportsMissingSecrets(t *testing.T) {
	_, rep, sec := build(t)
	assert.Equal(t, []string{secrets.ServiceKey("grafana", "GRAFANA_ADMIN_PASSWORD")}, rep.MissingSecrets)
	assert.NotContains(t, sec.Services, "grafana")
}

func TestImport_AddsTCPToRawPorts(t *testing.T) {
	dst, rep, _ := build(t)
	cfg, err := config.Load(filepath.Join(dst, "services/gitea/config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, []string{"tcp"}, cfg.Ports["22"].ProtocolList())
	assert.Equal(t, 2222, cfg.Ports["22"].Listen)
	assert.Equal(t, 2222, cfg.Ports["22"].Port)
	assert.Equal(t, []string{"tcp", "udp"}, cfg.Ports["default"].ProtocolList(), "HTTP ports are left alone")
	require.NotNil(t, cfg.Databases.Content, "the rest of the file survives")
	assert.Contains(t, strings.Join(rep.Changed, "\n"), "gitea: port 22 (listen 2222) is tcp only now")

	// The list form: a listen port with no protocol gets /tcp.
	old := oldDir(t)
	put(t, filepath.Join(old, "services/gitea/config.yaml"), "# keep me\nports:\n  - 3000\n  - 2222:2222\n  - 53:53/udp\n")
	dst = t.TempDir()
	rep, _, err = Build(old, dst, minisKeyring(t))
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(dst, "services/gitea/config.yaml")) //nolint:gosec // test
	require.NoError(t, err)
	assert.Contains(t, string(b), "# keep me")
	assert.Contains(t, string(b), "2222:2222/tcp")
	assert.Contains(t, string(b), "53:53/udp")
	assert.Contains(t, strings.Join(rep.Changed, "\n"), "gitea: port 2222:2222 is tcp only now")
}

func TestImport_RendersExposure(t *testing.T) {
	dst, _, _ := build(t)
	sites, err := os.ReadFile(configgen.SitesFile(dst, "gitea"))
	require.NoError(t, err)
	assert.Contains(t, string(sites), "gitea.{$HOME_SUBDOMAIN}.{$DOMAIN}")
	l4, err := os.ReadFile(configgen.L4File(dst, "gitea"))
	require.NoError(t, err)
	assert.Contains(t, string(l4), ":2222")
	assert.NotContains(t, string(l4), "udp/", "the raw port is tcp only")
	assert.FileExists(t, configgen.SitesFile(dst, "glance"))

	// vaultwarden was migrated from its legacy conf.d file.
	st, err := exposure.Load(dst, "vaultwarden")
	require.NoError(t, err)
	assert.Equal(t, []string{"ts"}, st.Layers)
	assert.FileExists(t, configgen.SitesFile(dst, "vaultwarden"))
	assert.NoDirExists(t, filepath.Join(dst, "caddy", "conf.d"))
}

func TestImport_ReportsUnservedLayers(t *testing.T) {
	dst, rep, _ := build(t)
	st, err := exposure.Load(dst, "immich")
	require.NoError(t, err)
	assert.Equal(t, []string{"ts", "tor"}, st.Layers, "tor stays recorded")
	assert.Contains(t, rep.Changed, "immich: layer tor kept but not served in cluster mode")
	assert.FileExists(t, configgen.SitesFile(dst, "immich"), "its ts layer is rendered")
}

func TestTar_ModesAndPrefix(t *testing.T) {
	dst, _, _ := build(t)
	var buf bytes.Buffer
	require.NoError(t, Tar(&buf, dst, "import-00"))
	tr := tar.NewReader(&buf)
	seen := map[string]int64{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(h.Name, "import-00/"), h.Name)
		assert.Zero(t, h.Uid)
		seen[h.Name] = h.Mode
	}
	assert.Equal(t, int64(0o600), seen["import-00/services/gitea/config.yaml"])
	assert.Equal(t, int64(0o700), seen["import-00/services/gitea/"])
}

// Every catalog service as installed by `homelab add` imports: its compose
// file loads for bind detection without any secret, and what it bind-mounts
// from its own dir travels.
func TestImport_WholeCatalog(t *testing.T) {
	catalog, err := filepath.Abs("../../assets/services")
	require.NoError(t, err)
	old := t.TempDir()
	put(t, filepath.Join(old, "config.yaml"), rootConfig)
	require.NoError(t, os.CopyFS(filepath.Join(old, "services"), os.DirFS(catalog)))
	dst := t.TempDir()
	rep, _, err := Build(old, dst, secrets.NewForTest(keyring.NewArrayKeyring(nil)))
	require.NoError(t, err)
	des, err := os.ReadDir(catalog)
	require.NoError(t, err)
	assert.Len(t, rep.Services, len(des))
	assert.FileExists(t, filepath.Join(dst, "services/glance/config/glance.yml"))
	assert.DirExists(t, filepath.Join(dst, "services/grafana/provisioning"))
	for _, c := range rep.Changed {
		assert.NotContains(t, c, "does not exist", "a catalog bind source is missing")
	}
}

// stage extracts the tree as the agent's CopyToContainer would, under a
// fresh staging dir in root, and returns its name.
func stage(t *testing.T, tree, root string) string {
	t.Helper()
	name, err := StagingName(nil)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, Tar(&buf, tree, name))
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return name
		}
		require.NoError(t, err)
		p := filepath.Join(root, filepath.FromSlash(h.Name))
		if h.Typeflag == tar.TypeDir {
			require.NoError(t, os.MkdirAll(p, os.FileMode(h.Mode)))
			continue
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, b, os.FileMode(h.Mode)))
	}
}
