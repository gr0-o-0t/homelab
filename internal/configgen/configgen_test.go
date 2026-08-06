package configgen

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
)

func TestLayerDisplayURL_Private(t *testing.T) {
	env := map[string]string{"HOME_SUBDOMAIN": "home", "DOMAIN": "example.com"}
	url := LayerDisplayURL("private", "gitea", env)
	assert.Equal(t, "https://gitea.home.example.com", url)
}

func TestLayerDisplayURL_PrivateFallback(t *testing.T) {
	env := map[string]string{} // no HOME_SUBDOMAIN or DOMAIN
	url := LayerDisplayURL("private", "gitea", env)
	assert.Equal(t, "https://gitea.{HOME_SUBDOMAIN}.{DOMAIN}", url)
}

func TestLayerDisplayURL_CF(t *testing.T) {
	env := map[string]string{"DOMAIN": "example.com"}
	url := LayerDisplayURL("cf", "gitea", env)
	assert.Equal(t, "https://gitea.example.com", url)
}

func TestLayerDisplayURL_Tor(t *testing.T) {
	url := LayerDisplayURL("tor", "gitea", nil)
	assert.Equal(t, "http://gitea.onion (via Caddy)", url)
}

func TestLayerDisplayURL_I2P(t *testing.T) {
	url := LayerDisplayURL("i2p", "gitea", nil)
	assert.Equal(t, "http://gitea.i2p (via Caddy)", url)
}

func TestLayerDisplayURL_Ygg(t *testing.T) {
	url := LayerDisplayURL("ygg", "gitea", nil)
	// No .ygg naming exists — the URL is the node address and allocated port,
	// neither of which this function can know. Real callers use
	// ygg.ServiceURL; this is only the fallback.
	assert.Contains(t, url, "homelab ygg status")
	assert.NotContains(t, url, "gitea.ygg")
}

func TestLayerDisplayURL_Unknown(t *testing.T) {
	url := LayerDisplayURL("unknown", "gitea", nil)
	assert.Equal(t, "", url)
}

func TestLayerDisplayURL_EmptyDisplayName(t *testing.T) {
	env := map[string]string{"HOME_SUBDOMAIN": "home", "DOMAIN": "example.com"}
	url := LayerDisplayURL("private", "", env)
	// When both env vars are set, they are resolved even with an empty display name.
	assert.Equal(t, "https://.home.example.com", url)
}

func TestLayerDisplayURL_CFWithoutDomain(t *testing.T) {
	env := map[string]string{} // no DOMAIN
	url := LayerDisplayURL("cf", "gitea", env)
	assert.Equal(t, "https://gitea.{DOMAIN}", url)
}

// ── PortEntries integration ────────────────────────────────────────────────

func TestLoadServiceInfo_NewFormat(t *testing.T) {
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services", "testapp")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcDir, "config.yaml"),
		[]byte("ports:\n  - web:8080\n  - admin:9090\n"),
		0o644,
	))

	info, err := LoadServiceInfo(dir, "testapp")
	require.NoError(t, err)
	require.True(t, info.HasVars)
	assert.Len(t, info.Ports, 2)
	assert.Equal(t, 8080, info.Ports["web"].Port)
	assert.Equal(t, 9090, info.Ports["admin"].Port)
}

func TestResolvePorts_NewFormat(t *testing.T) {
	ports := config.PortEntries{
		"web":   {Port: 3000},
		"admin": {Port: 9090},
	}
	result, err := ResolvePorts(ports, nil)
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, "admin", result[0].Name)
	assert.Equal(t, 9090, result[0].Port)
	assert.Equal(t, "web", result[1].Name)
	assert.Equal(t, 3000, result[1].Port)
}

// The three declaration forms, as they appear in a site address. See
// config.PortEntry for the grammar.
func TestSiteAddress_DeclarationForms(t *testing.T) {
	bare := PortSelection{Name: "default", Port: 8080}
	assert.Equal(t, "gitea.{$HOME_SUBDOMAIN}.{$DOMAIN}", siteAddress("gitea", "private", bare))

	mapped := PortSelection{Name: "22", Port: 22, Listen: 22}
	assert.Equal(t, "gitea.{$HOME_SUBDOMAIN}.{$DOMAIN}:22", siteAddress("gitea", "private", mapped))

	named := PortSelection{Name: "vault", Port: 80, Subdomain: "vault"}
	assert.Equal(t, "vault.{$HOME_SUBDOMAIN}.{$DOMAIN}", siteAddress("vaultwarden", "private", named),
		"a declared subdomain replaces the service name, it does not prefix it")

	assert.Equal(t, "vault.{$DOMAIN}", siteAddress("vaultwarden", "cf", named))
}

func TestBuildBlock_PrivateDefaultPort(t *testing.T) {
	block, err := buildBlock("private", "gitea", "gitea", PortSelection{Name: "default", Port: 3000, Protocol: "tcp"})
	require.NoError(t, err)
	assert.Equal(t, "private", block.Extension)
	assert.Equal(t, "default", block.PortName)
	assert.Contains(t, block.Content, "gitea.{$HOME_SUBDOMAIN}.{$DOMAIN}")
	assert.Contains(t, block.Content, "import wildcard_tls")
	assert.Contains(t, block.Content, "reverse_proxy gitea:3000")
}

func TestBuildBlock_CFNamedPort(t *testing.T) {
	block, err := buildBlock("cf", "gitea", "gitea", PortSelection{Name: "web", Port: 8080, Protocol: "tcp"})
	require.NoError(t, err)
	assert.Equal(t, "cf", block.Extension)
	assert.Contains(t, block.Content, "http://gitea.{$DOMAIN}")
	assert.Contains(t, block.Content, "reverse_proxy gitea:8080")
	assert.NotContains(t, block.Content, "import wildcard_tls")
}

// Mesh site addresses must carry the http:// scheme. Without it Caddy turns on
// automatic HTTPS for .i2p/.onion/.ygg: it binds :443, serves a redirect on
// :80 (which is the port the mesh layer actually dials), and burns ACME
// attempts on a name no CA will sign.
func TestBuildBlock_MeshLayersAreHTTPOnly(t *testing.T) {
	for ext, want := range map[string]string{
		"i2p": "http://mysvc.{$HOME_SUBDOMAIN}.i2p {",
	} {
		block, err := buildBlock(ext, "mysvc", "mysvc", PortSelection{Name: "default", Port: 80, Protocol: "tcp"})
		require.NoError(t, err, ext)
		assert.Contains(t, block.Content, want)
		assert.Contains(t, block.Content, "reverse_proxy mysvc:80")
	}
}

func TestBuildRoutesBlock_MeshLayersAreHTTPOnly(t *testing.T) {
	for ext, want := range map[string]string{
		"i2p": "http://appflowy.{$HOME_SUBDOMAIN}.i2p {",
	} {
		content, err := buildRoutesBlock(ext, "appflowy", "reverse_proxy appflowy:80\n")
		require.NoError(t, err, ext)
		assert.Contains(t, content, want)
	}
}

// Yggdrasil has no naming, so there is no `<name>.ygg` host to match on: the
// block is port-addressed and written by the ygg layer, which is the only
// thing that knows the port. Empty content is the signal to skip the write.
func TestBuildBlock_YggIsWrittenByItsLayer(t *testing.T) {
	block, err := buildBlock("ygg", "mysvc", "mysvc", PortSelection{Name: "web", Port: 8080, Protocol: "tcp"})
	require.NoError(t, err)
	assert.Empty(t, block.Content)
	assert.Equal(t, 8080, block.Port, "the port must still reach the layer")

	routes, err := buildRoutesBlock("ygg", "appflowy", "reverse_proxy appflowy:80\n")
	require.NoError(t, err)
	assert.Empty(t, routes)
}

func TestWrapSiteBlock(t *testing.T) {
	got := WrapSiteBlock(":9001", "# header comment\n\nreverse_proxy svc:80\n")
	assert.Equal(t, ":9001 {\n\treverse_proxy svc:80\n}\n", got)
}

func TestBuildBlock_DisplayNameDiffers(t *testing.T) {
	// displayName differs from svcName (--name override)
	block, err := buildBlock("private", "My App", "my-app", PortSelection{Name: "web", Port: 3000, Protocol: "tcp"})
	require.NoError(t, err)
	assert.Contains(t, block.Content, "My App.{$HOME_SUBDOMAIN}.{$DOMAIN}")
	assert.Contains(t, block.Content, "reverse_proxy my-app:3000")
}

func TestBuildBlock_UnknownExtension(t *testing.T) {
	_, err := buildBlock("unknown", "svc", "svc", PortSelection{Name: "default", Port: 80, Protocol: "tcp"})
	assert.ErrorContains(t, err, "unknown extension")
}

// Two i2p ports must not produce byte-identical blocks: combined with
// per-port filenames, that leaves two files claiming one host, which Caddy
// rejects at validate time.
func TestBuildBlock_I2PPerPortAddressesAreDistinct(t *testing.T) {
	web, err := buildBlock("i2p", "svc", "svc",
		PortSelection{Name: "default", Port: 8080, Protocol: "tcp"})
	require.NoError(t, err)
	admin, err := buildBlock("i2p", "svc", "svc",
		PortSelection{Name: "admin", Port: 9090, Subdomain: "admin", Protocol: "tcp"})
	require.NoError(t, err)

	assert.Contains(t, web.Content, "http://svc.{$HOME_SUBDOMAIN}.i2p {")
	assert.Contains(t, admin.Content, "http://admin.{$HOME_SUBDOMAIN}.i2p {")
	assert.NotEqual(t, web.Content, admin.Content)
}

// Tor joins ygg in writing its own Caddy config: a .onion is a hash of a key
// tor generates, so it cannot be templated from a service name, and nothing
// rewrites the Host header on the way in the way i2pd's hostoverride does.
func TestBuildBlock_TorIsWrittenByItsLayer(t *testing.T) {
	block, err := buildBlock("tor", "svc", "svc",
		PortSelection{Name: "default", Port: 8080, Protocol: "tcp"})
	require.NoError(t, err)
	assert.Empty(t, block.Content)

	routes, err := buildRoutesBlock("tor", "appflowy", "reverse_proxy appflowy:80\n")
	require.NoError(t, err)
	assert.Empty(t, routes)
}

// A mesh layer delivers to Caddy on :80 and nowhere else — i2pd's tunnel and
// tor's HiddenServicePort both target it. A port declared with its own listen
// port (22:22) therefore gets no mesh block, rather than one that sits there
// never receiving a request.
func TestBuildBlock_MeshSkipsExplicitListenPorts(t *testing.T) {
	for _, ext := range []string{"i2p"} {
		block, err := buildBlock(ext, "forgejo", "forgejo",
			PortSelection{Name: "22", Port: 22, Listen: 22, Protocol: "tcp"})
		require.NoError(t, err, ext)
		assert.Empty(t, block.Content, ext)
	}
}

// Caddy speaks HTTP; nothing here proxies datagrams. A udp port is recorded
// for compose and skipped for routing.
func TestBuildBlock_UDPGetsNoSiteBlock(t *testing.T) {
	block, err := buildBlock("private", "adguardhome", "adguardhome",
		PortSelection{Name: "53", Port: 53, Listen: 53, Protocol: "udp"})
	require.NoError(t, err)
	assert.Empty(t, block.Content)
}

// The ygg equivalent of this guard lives in internal/network/ygg: distinct
// ports come from the layer's allocator, not from the site address.

// ── WriteFile / RemoveFile filename scheme ───────────────────────────────────

func TestBlockFilename_DefaultPort(t *testing.T) {
	assert.Equal(t, "svc", PortFileName("svc", "default"))
	assert.Equal(t, "svc", PortFileName("svc", "web"))
	assert.Equal(t, "svc", PortFileName("svc", ""))
}

func TestBlockFilename_NamedPort(t *testing.T) {
	assert.Equal(t, "svc-ssh", PortFileName("svc", "ssh"))
}

func TestWriteFile_MultiPortPrivate_DoesNotClobber(t *testing.T) {
	// Regression test for the reproduced bug: WriteFile used to collapse
	// every private/cf filename to "<svc>.conf" regardless of port name, so
	// a second port's write silently overwrote the first.
	dir := t.TempDir()
	require.NoError(t, WriteFile(dir, "private", "svc", "web", "web-block\n"))
	require.NoError(t, WriteFile(dir, "private", "svc", "ssh", "ssh-block\n"))

	webData, err := os.ReadFile(filepath.Join(dir, "caddy", "conf.d", "svc.conf"))
	require.NoError(t, err, "default/web port should keep its own file")
	assert.Equal(t, "web-block\n", string(webData))

	sshData, err := os.ReadFile(filepath.Join(dir, "caddy", "conf.d", "svc-ssh.conf"))
	require.NoError(t, err, "second port should get its own file instead of overwriting the first")
	assert.Equal(t, "ssh-block\n", string(sshData))
}

func TestWriteFile_MultiPortCF_DoesNotClobber(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteFile(dir, "cf", "svc", "web", "web-block\n"))
	require.NoError(t, WriteFile(dir, "cf", "svc", "ssh", "ssh-block\n"))

	_, err := os.Stat(filepath.Join(dir, "caddy", "conf.d-cf", "svc.conf"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "caddy", "conf.d-cf", "svc-ssh.conf"))
	require.NoError(t, err, "cf should follow the same per-port scheme as the other extensions")
}

func TestRemoveAllPortFiles_RemovesEveryDeclaredPort(t *testing.T) {
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services", "svc")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcDir, "config.yaml"),
		[]byte("ports:\n  - web:8080\n  - ssh:22\n"),
		0o644,
	))

	require.NoError(t, WriteFile(dir, "private", "svc", "web", "web-block\n"))
	require.NoError(t, WriteFile(dir, "private", "svc", "ssh", "ssh-block\n"))

	require.NoError(t, RemoveAllPortFiles(dir, "private", "svc"))

	_, err := os.Stat(filepath.Join(dir, "caddy", "conf.d", "svc.conf"))
	assert.True(t, os.IsNotExist(err), "default-port file should be removed")
	_, err = os.Stat(filepath.Join(dir, "caddy", "conf.d", "svc-ssh.conf"))
	assert.True(t, os.IsNotExist(err), "per-port file should also be removed, not orphaned")
}

func TestRemoveAllPortFiles_NoPortsDeclared_FallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteFile(dir, "private", "svc", "", "content\n"))
	require.NoError(t, RemoveAllPortFiles(dir, "private", "svc"))
	_, err := os.Stat(filepath.Join(dir, "caddy", "conf.d", "svc.conf"))
	assert.True(t, os.IsNotExist(err))
}

// The ygg layer names its socat forwarders with this same function. If the two
// ever diverge again, enable writes files that disable cannot find.
func TestPortFileName_IsTheOneNamingRule(t *testing.T) {
	assert.Equal(t, "svc", PortFileName("svc", "default"))
	assert.Equal(t, "svc", PortFileName("svc", "web"))
	assert.Equal(t, "svc", PortFileName("svc", ""))
	assert.Equal(t, "svc-ssh", PortFileName("svc", "ssh"))
}

// An eepsite is namespaced under the home subdomain, matching the tailnet
// name. A bare <service>.i2p is a name in the global I2P namespace that anyone
// can register — and a browser asking for one reached a stranger's site,
// because nothing publishes ours under it.
func TestI2PHost_NamespacedUnderHomeSubdomain(t *testing.T) {
	assert.Equal(t, "searxng.leno.i2p", I2PHost("searxng", "leno"))
	assert.Equal(t, "searxng.{$HOME_SUBDOMAIN}.i2p", I2PHost("searxng", HomeSubdomainVar))

	// No home subdomain configured: fall back rather than emit "searxng..i2p".
	assert.Equal(t, "searxng.i2p", I2PHost("searxng", ""))
}
