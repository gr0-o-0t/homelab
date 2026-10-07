package configgen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
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

// generate resolves svc and renders it on each layer, keyed by layer name.
func generate(t *testing.T, root, svc, display string, ls ...network.NetworkLayer) map[string]configgen.CaddyBlock {
	t.Helper()
	e, err := configgen.Resolve(root, svc, display, nil)
	require.NoError(t, err)
	out := map[string]configgen.CaddyBlock{}
	for _, l := range ls {
		blocks, err := configgen.Render(l, e)
		require.NoError(t, err)
		require.Len(t, blocks, 1, "%s: one block per layer, not one per port", l.Name())
		out[l.Name()] = blocks[0]
	}
	return out
}

// The bug this exists for: a multi-route service used to get
// `reverse_proxy <svc>:<port>` on every layer, so on cf/i2p/tor/ygg everything
// except "/" was unreachable. All layers must now carry every route. (tor and
// ygg render through the same function; their golden tests cover them.)
func TestRender_RoutesAppliedToEveryLayer(t *testing.T) {
	root := t.TempDir()
	writeRoutesService(t, root, "appflowy", twoRoutes)

	wantAddress := map[string]string{
		"ts":  "appflowy.{$HOME_SUBDOMAIN}.{$DOMAIN} {",
		"cf":  "http://appflowy.{$DOMAIN} {",
		"i2p": "http://appflowy.{$HOME_SUBDOMAIN}.i2p {",
	}
	for name, b := range generate(t, root, "appflowy", "", hosts...) {
		assert.Contains(t, b.Content, wantAddress[name], "%s block should open with its own site address", name)
		assert.Contains(t, b.Content, "reverse_proxy svc-api:8000", "%s block lost the /api route", name)
		assert.Contains(t, b.Content, "reverse_proxy svc:80", "%s block lost the catch-all route", name)
		want := 0
		if name == "ts" {
			want = 1
		}
		assert.Equal(t, want, strings.Count(b.Content, "import wildcard_tls"),
			"%s: wildcard_tls belongs to the private layer only", name)
	}
}

// Routes make `ports:` optional — path routing lives inside the block, so
// there is nothing to fan out over.
func TestResolve_RoutesWithoutDeclaredPorts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "services", "noports")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("vars: {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, configgen.RoutesFileName),
		[]byte("reverse_proxy noports:3000\n"), 0o600))

	e, err := configgen.Resolve(root, "noports", "", nil)
	require.NoError(t, err, "a routes file should not require a ports section")
	require.Len(t, e.Ports, 1)
	assert.Equal(t, 80, e.Ports[0].Port, "falls back to 80 for layer bookkeeping")
	generate(t, root, "noports", "", i2pl)
}

// A service with no ports and no routes cannot be routed, and says so.
func TestResolve_HeadlessServiceIsAnError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "services", "postgres")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("vars: {}\n"), 0o600))
	_, err := configgen.Resolve(root, "postgres", "", nil)
	assert.ErrorContains(t, err, "no ports defined")
}

// --name has to rename the vhost without touching the upstreams, which stay
// whatever the routes body says.
func TestRender_RoutesHonourDisplayName(t *testing.T) {
	root := t.TempDir()
	writeRoutesService(t, root, "appflowy", twoRoutes)

	for name, b := range generate(t, root, "appflowy", "notes", ts, i2pl) {
		assert.Contains(t, b.Content, "notes", "%s should use the display name", name)
		assert.NotContains(t, b.Content, "appflowy.", "%s should not use the service name as vhost", name)
		assert.Contains(t, b.Content, "reverse_proxy svc:80", "upstreams must be untouched")
	}
}

// Generated output has to be a syntactically balanced site block.
func TestRender_RoutesBlockIsBalanced(t *testing.T) {
	root := t.TempDir()
	writeRoutesService(t, root, "appflowy", twoRoutes)

	content := generate(t, root, "appflowy", "", ts)["ts"].Content
	assert.Equal(t, strings.Count(content, "{"), strings.Count(content, "}"),
		"unbalanced braces would break the whole Caddyfile:\n%s", content)
	assert.True(t, strings.HasSuffix(content, "}\n"), "block must be closed")
}

// The file-level header describes the file, not the routing, so repeating it in
// all five generated blocks just buries the routes. Comments attached to a
// route must survive, though — those are the ones worth reading at 3am.
func TestRender_StripsFileHeaderKeepsRouteComments(t *testing.T) {
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

	content := generate(t, root, "svc", "", i2pl)["i2p"].Content
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

	require.NoError(t, configgen.WriteFile(root, "conf.d-i2p", "appflowy", "", "appflowy.i2p {\n}\n"))
	path := filepath.Join(configgen.ConfigDir(root, "conf.d-i2p"), "appflowy.conf")
	require.FileExists(t, path)

	require.NoError(t, configgen.RemoveAllPortFiles(root, "conf.d-i2p", "appflowy"))
	assert.NoFileExists(t, path, "disable left the generated layer config behind")
}
