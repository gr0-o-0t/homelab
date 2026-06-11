package tor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/groot/homelab/internal/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func noopReload() error { return nil }

func TestLayer_Identity(t *testing.T) {
	l := New("/test/repo", nil)
	assert.Equal(t, "tor", l.Name())
	assert.Equal(t, "Tor onion service proxy", l.Label())
	assert.Equal(t, "tor", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil)
	dir := l.CaddyConfigDir("/home/user/.config/homelab")
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-tor", dir)
}

func TestLayer_Enable_WritesCaddyConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	svcName := "gitea"
	ports := []network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}

	err := l.Enable(svcName, svcName, network.ServiceInfo{}, ports)
	require.NoError(t, err)

	caddyPath := filepath.Join(root, "caddy", "conf.d-tor", svcName+".conf")
	data, err := os.ReadFile(caddyPath)
	require.NoError(t, err, "Caddy config should exist")
	assert.Contains(t, string(data), "gitea.onion")
	assert.Contains(t, string(data), "reverse_proxy gitea:3000")
}

func TestLayer_Enable_WritesTorrcConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)

	torrcPath := filepath.Join(root, "tor", "torrc.d", "gitea.conf")
	data, err := os.ReadFile(torrcPath)
	require.NoError(t, err, "Torrc config should exist")
	assert.Contains(t, string(data), "HiddenServiceDir /var/lib/tor/hidden_service/gitea")
	assert.Contains(t, string(data), "HiddenServicePort 80 gitea:3000")

	// Verify hidden service directory was pre-created so Docker doesn't
	// auto-create it as root:root (tor runs non-root inside container).
	hsDir := filepath.Join(root, "tor", "hidden_service", "gitea")
	fi, err := os.Stat(hsDir)
	require.NoError(t, err, "Hidden service dir should exist")
	assert.True(t, fi.IsDir(), "Should be a directory")
}

func TestLayer_Disable_RemovesConfigs(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))

	require.NoError(t, l.Disable("gitea"))

	caddyPath := filepath.Join(root, "caddy", "conf.d-tor", "gitea.conf")
	_, err := os.Stat(caddyPath)
	assert.True(t, os.IsNotExist(err), "Caddy config should be removed")

	torrcPath := filepath.Join(root, "tor", "torrc.d", "gitea.conf")
	_, err = os.Stat(torrcPath)
	assert.True(t, os.IsNotExist(err), "Torrc config should be removed")
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	err := l.Disable("nonexistent")
	assert.NoError(t, err, "Disable should be idempotent")
}

func TestLayer_GenerateCaddyBlock(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	err := l.Enable("test", "test", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 8080, Protocol: "tcp"}})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(root, "caddy", "conf.d-tor", "test.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "test.onion")
	assert.Contains(t, string(data), "reverse_proxy test:8080")
}
