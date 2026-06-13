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
	assert.Equal(t, "http://gitea.ygg", url)
}

func TestLayerDisplayURL_IPFS(t *testing.T) {
	url := LayerDisplayURL("ipfs", "gitea", nil)
	assert.Equal(t, "ipfs://gitea", url)
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
