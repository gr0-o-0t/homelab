package i2p

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
	assert.Equal(t, "i2p", l.Name())
	assert.Equal(t, "I2P router + eepsite proxy", l.Label())
	assert.Equal(t, "i2p", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil)
	dir := l.CaddyConfigDir("/home/user/.config/homelab")
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-i2p", dir)
}

func TestLayer_Enable_WritesCaddyConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)

	caddyPath := filepath.Join(root, "caddy", "conf.d-i2p", "gitea.conf")
	data, err := os.ReadFile(caddyPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "gitea.i2p")
	assert.Contains(t, string(data), "reverse_proxy gitea:3000")
}

func TestLayer_Enable_WritesTunnelConfig(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	l := newForTest(root, noopReload)

	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)

	tunPath := filepath.Join(root, "i2p", "tunnels.conf")
	data, err := os.ReadFile(tunPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "[gitea]")
	assert.Contains(t, string(data), "host = caddy")
	assert.Contains(t, string(data), "port = 80")
	assert.Contains(t, string(data), "hostoverride = gitea.i2p")
}

func TestLayer_Enable_DuplicateTunnel(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	l := newForTest(root, noopReload)

	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))

	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	assert.Error(t, err, "Duplicate tunnel should error")
	assert.Contains(t, err.Error(), "already exists")
}

func TestLayer_Disable_RemovesConfigs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	l := newForTest(root, noopReload)

	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))

	require.NoError(t, l.Disable("gitea"))

	_, err := os.Stat(filepath.Join(root, "caddy", "conf.d-i2p", "gitea.conf"))
	assert.True(t, os.IsNotExist(err), "Caddy config should be removed")

	tunPath := filepath.Join(root, "i2p", "tunnels.conf")
	data, err := os.ReadFile(tunPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "[gitea]")
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)
	err := l.Disable("nonexistent")
	assert.NoError(t, err)
}

func TestLayer_ParseTunnels(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	// Write a test tunnels.conf
	tunPath := filepath.Join(root, "i2p", "tunnels.conf")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	content := `[gitea]
type = http
host = caddy
port = 80
hostoverride = gitea.i2p
keys = gitea.dat
`
	require.NoError(t, os.WriteFile(tunPath, []byte(content), 0o600))

	tunnels, err := l.parseTunnels()
	require.NoError(t, err)
	require.Len(t, tunnels, 1)
	assert.Equal(t, "gitea", tunnels[0].Name)
	assert.Equal(t, "caddy", tunnels[0].Host)
	assert.Equal(t, "80", tunnels[0].Port)
	assert.Equal(t, "gitea.i2p", tunnels[0].HostOverride)
}

func TestLayer_SectionRange(t *testing.T) {
	lines := []string{
		"",
		"[gitea]",
		"type = http",
		"host = caddy",
		"",
		"[other]",
		"type = http",
	}
	start, end, found := sectionRange(lines, "gitea")
	assert.True(t, found)
	assert.Equal(t, 1, start)
	assert.Equal(t, 5, end) // includes trailing blank
}
