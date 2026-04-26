package assets_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	requiredFiles := []string{"docker-compose.yml", "caddy.conf", "caddy-pub.conf", "config.yaml"}

	for _, svc := range services {
		for _, file := range requiredFiles {
			path := "services/" + svc + "/" + file
			_, err := assets.CatalogFS.Open(path)
			require.NoError(t, err, "service %s should have %s", svc, file)
		}
	}
}