package cf

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
	assert.Equal(t, "cf", l.Name())
	assert.Equal(t, "Cloudflare Tunnel", l.Label())
	assert.Equal(t, "cloudflared", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil)
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-cf", l.CaddyConfigDir("/home/user/.config/homelab"))
}

func TestLayer_Enable_WritesCaddyConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)
	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)
	data, _ := os.ReadFile(filepath.Join(root, "caddy", "conf.d-cf", "gitea.conf"))
	assert.Contains(t, string(data), "http://gitea.{$DOMAIN}")
	assert.Contains(t, string(data), "reverse_proxy gitea:3000")
}

func TestLayer_Disable_RemovesConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)
	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))
	require.NoError(t, l.Disable("gitea"))
	_, err := os.Stat(filepath.Join(root, "caddy", "conf.d-cf", "gitea.conf"))
	assert.True(t, os.IsNotExist(err))
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	l := newForTest(t.TempDir(), noopReload)
	assert.NoError(t, l.Disable("nonexistent"))
}
