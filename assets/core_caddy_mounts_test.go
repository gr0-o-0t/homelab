package assets_test

import (
	"regexp"
	"testing"

	"github.com/groot/homelab/assets"
	"github.com/stretchr/testify/require"
)

// TestCaddyfileImportsAreMounted checks that every conf.d* directory the
// Caddyfile imports is also bind-mounted into the caddy container.
//
// This is the failure the whole i2p/tor/ygg routing stack was built on top of:
// the Caddyfile imported conf.d-i2p, conf.d-tor and conf.d-ygg, but the compose
// file mounted only conf.d and conf.d-cf. Caddy treats an import glob matching
// no files as a no-op (not an error), so `homelab enable x --i2p` wrote config,
// `homelab validate` passed, Caddy reloaded happily — and the layer never
// routed a single request.
func TestCaddyfileImportsAreMounted(t *testing.T) {
	caddyfile, err := assets.CoreFS.ReadFile("caddy/Caddyfile")
	require.NoError(t, err)
	compose, err := assets.CoreFS.ReadFile("core/docker-compose.yml")
	require.NoError(t, err)

	imports := regexp.MustCompile(`(?m)^\s*import\s+(/homelab/caddy/[^/\s]+)/`).
		FindAllStringSubmatch(string(caddyfile), -1)
	require.NotEmpty(t, imports, "no imports found — did the Caddyfile move?")

	for _, m := range imports {
		containerPath := m[1]
		require.Contains(t, string(compose), ":"+containerPath+":ro",
			"Caddyfile imports %s but core/docker-compose.yml never mounts it — "+
				"the import silently matches nothing", containerPath)
	}
}

// TestCaddyImportDirsExist checks each imported dir ships in CoreFS. An absent
// directory is created by Docker at first start, owned by root, which then
// makes configgen's writes fail with EACCES.
func TestCaddyImportDirsExist(t *testing.T) {
	caddyfile, err := assets.CoreFS.ReadFile("caddy/Caddyfile")
	require.NoError(t, err)

	imports := regexp.MustCompile(`(?m)^\s*import\s+/homelab/caddy/([^/\s]+)/`).
		FindAllStringSubmatch(string(caddyfile), -1)

	for _, m := range imports {
		entries, err := assets.CoreFS.ReadDir("caddy/" + m[1])
		require.NoError(t, err, "caddy/%s is imported but not embedded", m[1])
		require.NotEmpty(t, entries,
			"caddy/%s is empty, so go:embed drops it — add a README", m[1])
	}
}
