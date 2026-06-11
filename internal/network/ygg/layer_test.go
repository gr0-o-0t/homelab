package ygg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/groot/homelab/internal/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func noopRestart() error { return nil }

func TestLayer_Identity(t *testing.T) {
	l := New("/test/repo", nil)
	assert.Equal(t, "ygg", l.Name())
	assert.Equal(t, "Yggdrasil mesh node", l.Label())
	assert.Equal(t, "yggdrasil", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil)
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-ygg", l.CaddyConfigDir("/home/user/.config/homelab"))
}

func TestLayer_Enable_WritesCaddyConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)
	data, _ := os.ReadFile(filepath.Join(root, "caddy", "conf.d-ygg", "gitea.conf"))
	assert.Contains(t, string(data), "gitea.ygg")
	assert.Contains(t, string(data), "reverse_proxy gitea:3000")
}

func TestLayer_Enable_WritesForwarder(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)
	data, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "gitea.forward"))
	assert.Contains(t, string(data), "PORT=3000")
	assert.Contains(t, string(data), "TARGET=gitea:3000")
}

func TestLayer_Disable_RemovesConfigs(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))
	require.NoError(t, l.Disable("gitea"))
	_, err := os.Stat(filepath.Join(root, "caddy", "conf.d-ygg", "gitea.conf"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "yggdrasil", "socat.d", "gitea.forward"))
	assert.True(t, os.IsNotExist(err))
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	l := newForTest(t.TempDir(), noopRestart)
	assert.NoError(t, l.Disable("nonexistent"))
}
