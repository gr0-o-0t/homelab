package assets_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
)

func TestCoreFS_ContainsExpectedFiles(t *testing.T) {
	entries, err := assets.CoreFS.ReadDir("caddy")
	require.NoError(t, err, "should be able to read caddy directory")

	caddyFiles := make(map[string]bool)
	for _, e := range entries {
		caddyFiles[e.Name()] = true
	}

	assert.True(t, caddyFiles["Caddyfile"], "should have Caddyfile")
	assert.True(t, caddyFiles["conf.d"], "should have conf.d subdirectory")

	confDEntries, err := assets.CoreFS.ReadDir("caddy/conf.d")
	require.NoError(t, err, "should be able to read caddy/conf.d")

	confDFiles := make(map[string]bool)
	for _, e := range confDEntries {
		confDFiles[e.Name()] = true
	}

	assert.True(t, confDFiles["README"], "conf.d should have README")
}

func TestCatalogFS_ContainsServices(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err, "should be able to read services directory")

	serviceNames := make([]string, len(entries))
	for i, e := range entries {
		serviceNames[i] = e.Name()
	}

	assert.Contains(t, serviceNames, "immich")
	assert.Contains(t, serviceNames, "jellyfin")
	assert.Contains(t, serviceNames, "uptime-kuma")
	assert.Contains(t, serviceNames, "vaultwarden")
}

func TestCatalogService_HasRequiredFiles(t *testing.T) {
	services := []string{"immich", "jellyfin", "uptime-kuma", "vaultwarden"}
	requiredFiles := []string{"docker-compose.yml", "caddy.conf", "caddy.cf.conf", "config.yaml"}

	for _, svc := range services {
		for _, file := range requiredFiles {
			path := "services/" + svc + "/" + file
			_, err := assets.CatalogFS.Open(path)
			require.NoError(t, err, "service %s should have %s", svc, file)
		}
	}
}

// ── whole-catalog regression guards ──────────────────────────────────────────
//
// These walk every entry in the catalog rather than a hand-picked sample —
// added after a review found two real, catalog-wide-scoped bugs: a service
// whose compose file used the ".yaml" extension the CLI doesn't look for
// (internal/run.ServiceComposeFile hardcodes ".yml"), and (checked, already
// clean) the possibility of a container_name or named volume colliding
// across two unrelated services, which would break if a user ever installed
// both — Docker names must be globally unique on the host.

type catalogComposeFile struct {
	Services map[string]struct {
		ContainerName string `yaml:"container_name"`
	} `yaml:"services"`
	Volumes map[string]struct {
		Name string `yaml:"name"`
	} `yaml:"volumes"`
}

func TestCatalogServices_ComposeFileUsesYmlExtension(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := "services/" + e.Name() + "/docker-compose.yml"
		_, err := assets.CatalogFS.Open(path)
		assert.NoError(t, err, "service %q must ship docker-compose.yml (not .yaml) — "+
			"internal/run.ServiceComposeFile hardcodes the .yml extension, so anything "+
			"else is silently unreachable by `homelab up`/`enable`", e.Name())
	}
}

func TestCatalogServices_ContainerNamesAndVolumeNamesAreGloballyUnique(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	containerNames := make(map[string]string) // name -> owning service
	volumeNames := make(map[string]string)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()
		data, err := assets.CatalogFS.ReadFile("services/" + svc + "/docker-compose.yml")
		if err != nil {
			continue // covered by TestCatalogServices_ComposeFileUsesYmlExtension
		}

		var cf catalogComposeFile
		require.NoError(t, yaml.Unmarshal(data, &cf), "service %q docker-compose.yml must parse as YAML", svc)

		for _, s := range cf.Services {
			if s.ContainerName == "" {
				continue
			}
			if owner, exists := containerNames[s.ContainerName]; exists {
				t.Errorf("container_name %q used by both %q and %q — would collide if both were installed",
					s.ContainerName, owner, svc)
			}
			containerNames[s.ContainerName] = svc
		}

		for _, v := range cf.Volumes {
			if v.Name == "" {
				continue
			}
			if owner, exists := volumeNames[v.Name]; exists {
				t.Errorf("volume name %q used by both %q and %q — would collide if both were installed",
					v.Name, owner, svc)
			}
			volumeNames[v.Name] = svc
		}
	}
}