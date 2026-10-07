package tailscale

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/groot/homelab/internal/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLayer_Identity(t *testing.T) {
	l := New("/test/repo", nil, nil)
	assert.Equal(t, "ts", l.Name())
	assert.Equal(t, "Tailscale mesh VPN", l.Label())
	assert.Equal(t, "tailscale", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil, nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil, nil)
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d", l.CaddyConfigDir("/home/user/.config/homelab"))
}

func TestLayer_Enable_Noop(t *testing.T) {
	l := New("/test/repo", nil, nil)
	assert.NoError(t, l.Enable("any", "any", network.ServiceInfo{}, nil))
}

func TestLayer_Disable_Noop(t *testing.T) {
	l := New("/test/repo", nil, nil)
	assert.NoError(t, l.Disable("any"))
}

// The URL names the host the generated block answers on, not the service.
func TestLayer_ServiceAddresses_UsesSiteHost(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "services", "vaultwarden")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("ports:\n  - vault:80\n"), 0o600))
	env := map[string]string{"HOME_SUBDOMAIN": "home", "DOMAIN": "example.com"}

	addrs := New(root, nil, nil).ServiceAddresses("vaultwarden", env)
	require.Len(t, addrs, 1)
	assert.Equal(t, "https://vault.home.example.com", addrs[0].URL)
}
