package configgen_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/cf"
	"github.com/groot/homelab/internal/network/i2p"
	"github.com/groot/homelab/internal/network/tailscale"
)

var (
	ts    = tailscale.New("", nil, nil)
	cfl   = cf.New("", nil, nil)
	i2pl  = i2p.New("", nil, nil)
	hosts = []network.NetworkLayer{ts, cfl, i2pl}
)

// render renders one port of svc on a layer and returns the blocks' content.
func render(t *testing.T, l network.NetworkLayer, host, svc string, p network.PortSelection) []string {
	t.Helper()
	blocks, err := configgen.Render(l, configgen.Exposure{Service: svc, Host: host, Ports: []network.PortSelection{p}})
	require.NoError(t, err)
	var out []string
	for _, b := range blocks {
		out = append(out, b.Content)
	}
	return out
}

// The declaration forms, as they appear in a site address. See
// config.PortEntry for the grammar.
func TestRender_DeclarationForms(t *testing.T) {
	bare := network.PortSelection{Name: "default", Port: 8080, Protocol: "tcp"}
	got := render(t, ts, "gitea", "gitea", bare)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "gitea.{$HOME_SUBDOMAIN}.{$DOMAIN} {")
	assert.Contains(t, got[0], "import wildcard_tls")
	assert.Contains(t, got[0], "reverse_proxy gitea:8080")

	named := network.PortSelection{Name: "vault", Port: 80, Subdomain: "vault", Protocol: "tcp"}
	assert.Contains(t, render(t, ts, "vaultwarden", "vaultwarden", named)[0], "vault.{$HOME_SUBDOMAIN}.{$DOMAIN} {",
		"a declared subdomain replaces the service name, it does not prefix it")
	assert.Contains(t, render(t, cfl, "vaultwarden", "vaultwarden", named)[0], "http://vault.{$DOMAIN} {")
}

func TestRender_CF(t *testing.T) {
	got := render(t, cfl, "gitea", "gitea", network.PortSelection{Name: "web", Port: 8080, Protocol: "tcp"})[0]
	assert.Contains(t, got, "http://gitea.{$DOMAIN}")
	assert.Contains(t, got, "reverse_proxy gitea:8080")
	assert.NotContains(t, got, "import wildcard_tls")
}

// Mesh site addresses must carry the http:// scheme. Without it Caddy turns on
// automatic HTTPS for .i2p: it binds :443, serves a redirect on :80 (which is
// the port the mesh layer actually dials), and burns ACME attempts on a name
// no CA will sign.
func TestRender_I2PIsHTTPOnly(t *testing.T) {
	got := render(t, i2pl, "mysvc", "mysvc", network.PortSelection{Name: "default", Port: 80, Protocol: "tcp"})[0]
	assert.Contains(t, got, "http://mysvc.{$HOME_SUBDOMAIN}.i2p {")
	assert.Contains(t, got, "reverse_proxy mysvc:80")
}

func TestRender_DisplayNameDiffers(t *testing.T) {
	got := render(t, ts, "My App", "my-app", network.PortSelection{Name: "web", Port: 3000, Protocol: "tcp"})[0]
	assert.Contains(t, got, "My App.{$HOME_SUBDOMAIN}.{$DOMAIN}")
	assert.Contains(t, got, "reverse_proxy my-app:3000")
}

// Two i2p ports must not produce byte-identical blocks: combined with
// per-port filenames, that leaves two files claiming one host, which Caddy
// rejects at validate time.
func TestRender_I2PPerPortAddressesAreDistinct(t *testing.T) {
	web := render(t, i2pl, "svc", "svc", network.PortSelection{Name: "default", Port: 8080, Protocol: "tcp"})[0]
	admin := render(t, i2pl, "svc", "svc", network.PortSelection{Name: "admin", Port: 9090, Subdomain: "admin", Protocol: "tcp"})[0]
	assert.Contains(t, web, "http://svc.{$HOME_SUBDOMAIN}.i2p {")
	assert.Contains(t, admin, "http://admin.{$HOME_SUBDOMAIN}.i2p {")
	assert.NotEqual(t, web, admin)
}

// A port declared with its own listen port (22:22) is raw TCP (ssh): an HTTPS
// reverse_proxy on :22, a plain-HTTP cf listener there, or an eepsite block i2pd
// never delivers to can never work. And Caddy speaks HTTP; nothing here proxies
// datagrams, so a udp port is recorded for compose and skipped for routing.
func TestRender_HostLayersSkipListenAndUDPPorts(t *testing.T) {
	for _, l := range hosts {
		assert.Empty(t, render(t, l, "forgejo", "forgejo",
			network.PortSelection{Name: "22", Port: 22, Listen: 22, Protocol: "tcp"}), l.Name())
		assert.Empty(t, render(t, l, "adguardhome", "adguardhome",
			network.PortSelection{Name: "53", Port: 53, Listen: 53, Protocol: "udp"}), l.Name())
	}
}

// Layers with one name per service outside Caddy (the i2p hostoverride, the cf
// DNS route) resolve the host with SiteHost; the block must agree.
func TestResolve_SiteHostMatchesRenderedAddress(t *testing.T) {
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services", "vaultwarden")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(svcDir, "config.yaml"), []byte("ports:\n  - vault:80\n"), 0o644))
	info, err := configgen.LoadServiceInfo(dir, "vaultwarden")
	require.NoError(t, err)

	for _, display := range []string{"", "vaultwarden"} {
		e, err := configgen.Resolve(dir, "vaultwarden", display, nil)
		require.NoError(t, err)
		cfBlocks, err := configgen.Render(cfl, e)
		require.NoError(t, err)
		assert.Contains(t, cfBlocks[0].Content, "http://vault.{$DOMAIN} {")
		i2pBlocks, err := configgen.Render(i2pl, e)
		require.NoError(t, err)
		assert.Contains(t, i2pBlocks[0].Content,
			"http://"+configgen.I2PHost(configgen.SiteHost(info, display), configgen.HomeSubdomainVar)+" {")
	}
}

// CurrentHost reads the enabled block, which records a --name the declaration
// cannot know about.
func TestCurrentHost_PrefersGeneratedBlock(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, "gitea", configgen.CurrentHost(dir, cfl, "gitea"), "nothing enabled, nothing declared")

	require.NoError(t, configgen.WriteFile(dir, "conf.d-cf", "gitea", "", "http://git.{$DOMAIN} {\n    reverse_proxy gitea:3000\n}\n"))
	assert.Equal(t, "git", configgen.CurrentHost(dir, cfl, "gitea"))
}

// GeneratedHost reads back every Host-routed layer, including a listen port on
// the address, and refuses addresses it did not write.
func TestGeneratedHost_PerLayer(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, "", configgen.GeneratedHost(dir, ts, "gitea"), "no block, no host")

	require.NoError(t, configgen.WriteFile(dir, "conf.d", "gitea", "", "git.{$HOME_SUBDOMAIN}.{$DOMAIN}:8443 {\n}\n"))
	require.NoError(t, configgen.WriteFile(dir, "conf.d-i2p", "gitea", "", "http://"+configgen.I2PHost("git", configgen.HomeSubdomainVar)+" {\n}\n"))
	require.NoError(t, configgen.WriteFile(dir, "conf.d-cf", "gitea", "", "http://example.org {\n}\n"))
	assert.Equal(t, "git", configgen.GeneratedHost(dir, ts, "gitea"))
	assert.Equal(t, "git", configgen.GeneratedHost(dir, i2pl, "gitea"))
	assert.Equal(t, "", configgen.GeneratedHost(dir, cfl, "gitea"))
	assert.Equal(t, "gitea", configgen.CurrentHost(dir, cfl, "gitea"), "unreadable block falls back to the declaration")
	assert.Equal(t, "git", configgen.CurrentHost(dir, ts, "gitea"))
}
