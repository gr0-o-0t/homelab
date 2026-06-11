package ipfs

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
	assert.Equal(t, "ipfs", l.Name())
	assert.Equal(t, "IPFS Kubo node", l.Label())
	assert.Equal(t, "ipfs", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil)
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-ipfs", l.CaddyConfigDir("/home/user/.config/homelab"))
}

func TestLayer_Enable_WritesGatewayCaddyConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)
	err := l.Enable("gateway", "ipfs.home", network.ServiceInfo{}, nil)
	require.NoError(t, err)
	data, _ := os.ReadFile(filepath.Join(root, "caddy", "conf.d-ipfs", "gateway.conf"))
	assert.Contains(t, string(data), "gateway.ipfs.home")
	assert.Contains(t, string(data), "reverse_proxy ipfs:8080")
}

func TestLayer_Disable_RemovesGatewayConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)
	require.NoError(t, l.Enable("gateway", "ipfs.home", network.ServiceInfo{}, nil))
	require.NoError(t, l.Disable("gateway"))
	_, err := os.Stat(filepath.Join(root, "caddy", "conf.d-ipfs", "gateway.conf"))
	assert.True(t, os.IsNotExist(err))
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	l := newForTest(t.TempDir(), noopReload)
	assert.NoError(t, l.Disable("nonexistent"))
}
