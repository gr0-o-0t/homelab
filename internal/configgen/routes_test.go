package configgen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
)

// writeRoutesService lays out a service dir with a config.yaml and a
// caddy.routes.conf, mimicking an installed multi-route service.
func writeRoutesService(t *testing.T, root, name, routes string) {
	t.Helper()
	dir := filepath.Join(root, "services", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("ports:\n  - 80\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, configgen.RoutesFileName),
		[]byte(routes), 0o600))
}

const twoRoutes = "handle /api/* {\n\treverse_proxy svc-api:8000\n}\n\nhandle {\n\treverse_proxy svc:80\n}\n"

// The bug this exists for: a multi-route service used to get
// `reverse_proxy <svc>:<port>` on every layer, so on cf/i2p/tor/ygg everything
// except "/" was unreachable. All layers must now carry every route.
func TestGenerate_RoutesAppliedToEveryLayer(t *testing.T) {
	root := t.TempDir()
	writeRoutesService(t, root, "appflowy", twoRoutes)

	// ygg and tor are absent: their addresses cannot be templated from a
	// service name, so those layers write their own blocks from the same
	// routes body — an allocated listening port for ygg, a generated .onion
	// for tor.
	exts := []string{"private", "cf", "i2p"}
	blocks, err := configgen.Generate(configgen.Request{
		ServiceName: "appflowy",
		Extensions:  exts,
		ConfigDir:   root,
	})
	require.NoError(t, err)
	require.Len(t, blocks, len(exts), "one block per layer, not one per port")

	wantAddress := map[string]string{
		"private": "appflowy.{$HOME_SUBDOMAIN}.{$DOMAIN} {",
		"cf":      "http://appflowy.{$DOMAIN} {",
		"i2p":     "http://appflowy.{$HOME_SUBDOMAIN}.i2p {",
	}

	for _, b := range blocks {
		assert.Contains(t, b.Content, wantAddress[b.Extension],
			"%s block should open with its own site address", b.Extension)
		assert.Contains(t, b.Content, "reverse_proxy svc-api:8000",
			"%s block lost the /api route", b.Extension)
		assert.Contains(t, b.Content, "reverse_proxy svc:80",
			"%s block lost the catch-all route", b.Extension)
		assert.Equal(t, 80, b.Port, "%s block should report the primary port", b.Extension)
		assert.Equal(t, 1, strings.Count(b.Content, "import wildcard_tls")+
			boolToInt(b.Extension != "private"),
			"%s: wildcard_tls belongs to the private layer only", b.Extension)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Routes make `ports:` optional — path routing lives inside the block, so
// there is nothing to fan out over.
func TestGenerate_RoutesWithoutDeclaredPorts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "services", "noports")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("vars: {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, configgen.RoutesFileName),
		[]byte("reverse_proxy noports:3000\n"), 0o600))

	blocks, err := configgen.Generate(configgen.Request{
		ServiceName: "noports",
		Extensions:  []string{"i2p"},
		ConfigDir:   root,
	})
	require.NoError(t, err, "a routes file should not require a ports section")
	require.Len(t, blocks, 1)
	assert.Equal(t, 80, blocks[0].Port, "falls back to 80 for layer bookkeeping")
}

// --name has to rename the vhost without touching the upstreams, which stay
// whatever the routes body says.
func TestGenerate_RoutesHonourDisplayName(t *testing.T) {
	root := t.TempDir()
	writeRoutesService(t, root, "appflowy", twoRoutes)

	blocks, err := configgen.Generate(configgen.Request{
		ServiceName: "appflowy",
		DisplayName: "notes",
		Extensions:  []string{"private", "i2p"},
		ConfigDir:   root,
	})
	require.NoError(t, err)

	for _, b := range blocks {
		assert.Contains(t, b.Content, "notes", "%s should use the display name", b.Extension)
		assert.NotContains(t, b.Content, "appflowy.", "%s should not use the service name as vhost", b.Extension)
		assert.Contains(t, b.Content, "reverse_proxy svc:80", "upstreams must be untouched")
	}
}

// Generated output has to be a syntactically balanced site block.
func TestGenerate_RoutesBlockIsBalanced(t *testing.T) {
	root := t.TempDir()
	writeRoutesService(t, root, "appflowy", twoRoutes)

	blocks, err := configgen.Generate(configgen.Request{
		ServiceName: "appflowy",
		Extensions:  []string{"private"},
		ConfigDir:   root,
	})
	require.NoError(t, err)

	content := blocks[0].Content
	assert.Equal(t, strings.Count(content, "{"), strings.Count(content, "}"),
		"unbalanced braces would break the whole Caddyfile:\n%s", content)
	assert.True(t, strings.HasSuffix(content, "}\n"), "block must be closed")
}

// The file-level header describes the file, not the routing, so repeating it in
// all five generated blocks just buries the routes. Comments attached to a
// route must survive, though — those are the ones worth reading at 3am.
func TestGenerate_StripsFileHeaderKeepsRouteComments(t *testing.T) {
	root := t.TempDir()
	writeRoutesService(t, root, "svc", strings.Join([]string{
		"# File header: wrapped per layer by homelab enable.",
		"# Second header line.",
		"",
		"# Auth route: prefix is stripped.",
		"handle_path /gotrue/* {",
		"\treverse_proxy svc-gotrue:9999",
		"}",
	}, "\n"))

	blocks, err := configgen.Generate(configgen.Request{
		ServiceName: "svc",
		Extensions:  []string{"i2p"},
		ConfigDir:   root,
	})
	require.NoError(t, err)

	content := blocks[0].Content
	assert.NotContains(t, content, "File header", "file-level header should not be copied out")
	assert.NotContains(t, content, "Second header line")
	assert.Contains(t, content, "# Auth route: prefix is stripped.",
		"a comment documenting a route must be preserved")
	assert.Contains(t, content, "reverse_proxy svc-gotrue:9999")
}

// RemoveAllPortFiles fans out over declared ports; a routes-driven service has
// a single file per layer regardless, so it must not be missed on disable.
func TestRemoveAllPortFiles_RoutesService(t *testing.T) {
	root := t.TempDir()
	writeRoutesService(t, root, "appflowy", twoRoutes)

	require.NoError(t, configgen.WriteFile(root, "i2p", "appflowy", "", "appflowy.i2p {\n}\n"))
	path := filepath.Join(configgen.ConfigDir(root, "i2p"), "appflowy.conf")
	require.FileExists(t, path)

	require.NoError(t, configgen.RemoveAllPortFiles(root, "i2p", "appflowy"))
	assert.NoFileExists(t, path, "disable left the generated layer config behind")
}
