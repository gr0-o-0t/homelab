package assets_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
	"github.com/groot/homelab/internal/config"
)

// bundledEngines maps an image-name fragment to the one catalog directory
// allowed to run it. Everything else declares a `databases:` dependency on
// that shared service and gets its own database/number/bucket inside it.
var bundledEngines = []struct{ fragment, owner string }{
	{"postgres", "postgres"},
	{"pgvecto", "postgres"},
	{"vchord", "postgres"},
	{"mysql", "mariadb"},
	{"mariadb", "mariadb"},
	{"mongo", ""}, // no shared mongo: nothing may bundle one
	{"redis", "redis"},
	{"valkey", "redis"},
	{"keydb", "redis"},
	{"dragonfly", "redis"},
	{"minio", "garage"},
	{"garage", "garage"},
}

// imageDefault resolves "${VAR:-default}" to its default, so a configurable
// image is judged by what it runs out of the box.
var imageVarDefault = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:?-([^}]*)\}`)

// imageRepo returns the repository part of an image reference, lowercased:
// "ghcr.io/tensorchord/vchord-postgres:pg18" → "tensorchord/vchord-postgres".
func imageRepo(image string) string {
	image = imageVarDefault.ReplaceAllString(image, "$1")
	image = strings.ToLower(image)
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	if parts := strings.SplitN(image, "/", 2); len(parts) == 2 && strings.ContainsAny(parts[0], ".:") {
		image = parts[1] // drop the registry host
	}
	return image
}

// One instance of each database, cache or object store, shared by every
// service: a service needing postgres, redis or S3 declares it under
// `databases:` and is isolated inside the shared instance. A compose file
// shipping its own copy is the duplication this guards against — netbox's
// private valkeys and appflowy's minio were the last two.
func TestCatalogServices_NoBundledDatabases(t *testing.T) {
	var bad []string
	for svc, cf := range loadCatalog(t) {
		for name, s := range cf.Services {
			repo := imageRepo(s.Image)
			for _, e := range bundledEngines {
				if strings.Contains(repo, e.fragment) && svc != e.owner {
					bad = append(bad, svc+"/"+name+" runs "+s.Image)
				}
			}
		}
	}
	sort.Strings(bad)
	assert.Empty(t, bad, "declare a shared dependency under config.yaml `databases:` "+
		"(postgres, mariadb, redis, s3) instead of bundling a database container")
}

func TestImageRepo(t *testing.T) {
	for in, want := range map[string]string{
		"${POSTGRES_IMAGE:-ghcr.io/tensorchord/vchord-postgres:pg18-v1.1.1}": "tensorchord/vchord-postgres",
		"redis:${REDIS_VERSION:-8-alpine}":                                   "redis",
		"docker.io/valkey/valkey:9-alpine":                                   "valkey/valkey",
		"minio/minio:RELEASE.2025-04-22T22-12-26Z":                           "minio/minio",
		"localhost:5000/foo/bar@sha256:abc":                                  "foo/bar",
	} {
		assert.Equal(t, want, imageRepo(in), in)
	}
}

// The allocator hands out 1..RedisDBCount-1; the shared redis must actually
// have that many databases, or SELECT fails for the highest numbers.
func TestRedis_DatabaseCountMatchesAllocator(t *testing.T) {
	data, err := assets.CatalogFS.ReadFile("services/redis/docker-compose.yml")
	require.NoError(t, err)
	var cf struct {
		Services map[string]struct {
			Command []string `yaml:"command"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(data, &cf))
	cmd := cf.Services["redis"].Command
	for i, arg := range cmd {
		if arg == "--databases" && i+1 < len(cmd) {
			assert.Equal(t, strconv.Itoa(config.RedisDBCount), cmd[i+1])
			return
		}
	}
	t.Fatalf("redis command %v has no --databases", cmd)
}

// Every logical key in a `databases:` env mapping must be one injectDBEnv
// knows for that type; an unknown key is silently ignored and the variable
// arrives empty.
func TestCatalogServices_DatabaseEnvKeysAreKnown(t *testing.T) {
	common := []string{"host", "port", "dsn"}
	known := map[config.DBType][]string{
		config.DBPostgres: {"user", "password", "database"},
		config.DBMariaDB:  {"user", "password", "database"},
		config.DBRedis:    {"password", "db"},
		config.DBS3:       {"endpoint", "region", "bucket", "access_key", "secret_key"},
	}
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)
	for _, e := range entries {
		dbs := catalogDatabases(t, e.Name())
		seenRedis := map[string]bool{}
		for _, d := range dbs {
			allowed, ok := known[d.Type]
			if !assert.True(t, ok, "service %q declares unknown type %q", e.Name(), d.Type) {
				continue
			}
			allowed = append(allowed, common...)
			for k := range d.Env {
				assert.Contains(t, allowed, k, "service %q: %s env key %q is not injected", e.Name(), d.Type, k)
			}
			if d.Type == config.DBRedis {
				assert.False(t, seenRedis[d.Name],
					"service %q: two redis declarations named %q would share one database number", e.Name(), d.Name)
				seenRedis[d.Name] = true
			}
		}
	}
}

func catalogDatabases(t *testing.T, svc string) config.ServiceDatabases {
	t.Helper()
	data, err := assets.CatalogFS.ReadFile("services/" + svc + "/config.yaml")
	if err != nil {
		return nil
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	dbs, err := cfg.ServiceDatabases()
	require.NoError(t, err, "service %q databases block must decode", svc)
	return dbs
}
