package assets_test

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
)

// CoreFS is only the Caddyfile internal/edge assembles from: the Compose
// core stack, its Dockerfiles and the per-layer conf.d* trees are gone.
func TestCoreFS_IsOnlyTheCaddyfile(t *testing.T) {
	var files []string
	require.NoError(t, fs.WalkDir(assets.CoreFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	}))
	assert.Equal(t, []string{"caddy/Caddyfile"}, files)
}

// docker-compose.yml is checked by TestCatalogServices_ComposeFileUsesYmlExtension.
func TestCatalogService_HasConfig(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_, err := assets.CatalogFS.Open("services/" + e.Name() + "/config.yaml")
		assert.NoError(t, err, "service %s should have config.yaml", e.Name())
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
