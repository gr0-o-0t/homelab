package configgen

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
)

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
	require.NoError(t, WriteFile(dir, "conf.d", "svc", "web", "web-block\n"))
	require.NoError(t, WriteFile(dir, "conf.d", "svc", "ssh", "ssh-block\n"))

	webData, err := os.ReadFile(filepath.Join(dir, "caddy", "conf.d", "svc.conf"))
	require.NoError(t, err, "default/web port should keep its own file")
	assert.Equal(t, "web-block\n", string(webData))

	sshData, err := os.ReadFile(filepath.Join(dir, "caddy", "conf.d", "svc-ssh.conf"))
	require.NoError(t, err, "second port should get its own file instead of overwriting the first")
	assert.Equal(t, "ssh-block\n", string(sshData))
}

func TestWriteFile_MultiPortCF_DoesNotClobber(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteFile(dir, "conf.d-cf", "svc", "web", "web-block\n"))
	require.NoError(t, WriteFile(dir, "conf.d-cf", "svc", "ssh", "ssh-block\n"))

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

	require.NoError(t, WriteFile(dir, "conf.d", "svc", "web", "web-block\n"))
	require.NoError(t, WriteFile(dir, "conf.d", "svc", "ssh", "ssh-block\n"))

	require.NoError(t, RemoveAllPortFiles(dir, "conf.d", "svc"))

	_, err := os.Stat(filepath.Join(dir, "caddy", "conf.d", "svc.conf"))
	assert.True(t, os.IsNotExist(err), "default-port file should be removed")
	_, err = os.Stat(filepath.Join(dir, "caddy", "conf.d", "svc-ssh.conf"))
	assert.True(t, os.IsNotExist(err), "per-port file should also be removed, not orphaned")
}

func TestRemoveAllPortFiles_NoPortsDeclared_FallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteFile(dir, "conf.d", "svc", "", "content\n"))
	require.NoError(t, RemoveAllPortFiles(dir, "conf.d", "svc"))
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

// Layers with one name per service outside Caddy (the i2p hostoverride, the cf
// DNS route) must resolve the same host Resolve puts in the site block; see
// render_test.go for the block side.
func TestSiteHost_MatchesGeneratedSiteAddress(t *testing.T) {
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services", "vaultwarden")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(svcDir, "config.yaml"),
		[]byte("ports:\n  - vault:80\n"), 0o644))

	info, err := LoadServiceInfo(dir, "vaultwarden")
	require.NoError(t, err)
	assert.Equal(t, "vault", SiteHost(info, ""), "declared subdomain")
	assert.Equal(t, "vault", SiteHost(info, "vaultwarden"), "service name is not an override")

	// --name wins over the service name on a port without a subdomain.
	plain := ServiceInfo{Name: "gitea", Ports: config.PortEntries{"default": {Port: 3000, Protocols: []string{"tcp"}}}}
	assert.Equal(t, "git", SiteHost(plain, "git"))
	assert.Equal(t, "gitea", SiteHost(plain, ""))
}
