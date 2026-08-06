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
	l := New("/test/repo", nil, nil)
	assert.Equal(t, "ygg", l.Name())
	assert.Equal(t, "Yggdrasil mesh node", l.Label())
	assert.Equal(t, "yggdrasil", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil, nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil, nil)
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-ygg", l.CaddyConfigDir("/home/user/.config/homelab"))
}

// Every layer used to hardcode an empty env, so their compose calls ran with no
// variables at all — visible as "WARN The TS_AUTHKEY variable is not set", and
// one `Layer.Start()` (a bare `--profile X up -d`, which targets the whole
// file) away from recreating the tailscale container with a blank auth key.
func TestLayer_Env_ComesFromInjectedFunc(t *testing.T) {
	l := New("/test/repo", nil, func() map[string]string {
		return map[string]string{"TS_AUTHKEY": "tskey-abc"}
	})
	assert.Equal(t, "tskey-abc", l.env()["TS_AUTHKEY"])

	// nil stays tolerated — tests build layers without one — but must not panic.
	assert.Empty(t, New("/test/repo", nil, nil).env())
}

// Unlike the other layers, ygg owns its Caddy config: the mesh has no naming,
// so Caddy routes by listening port and only this layer knows the port.

func TestLayer_Enable_WritesForwarderAndCaddyBlock(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)

	data, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "gitea.forward"))
	assert.Contains(t, string(data), "PORT=9000")
	// Via Caddy, not straight at the service container: "tailscale" is where
	// Caddy listens (it shares that container's network namespace).
	assert.Contains(t, string(data), "TARGET=tailscale:9000")
	assert.NotContains(t, string(data), "TARGET=gitea:3000")

	block, err := os.ReadFile(filepath.Join(root, "caddy", "conf.d-ygg", "gitea.conf"))
	require.NoError(t, err, "ygg layer must write its own Caddy site block")
	assert.Contains(t, string(block), ":9000 {")
	assert.Contains(t, string(block), "reverse_proxy gitea:3000")
}

// Mesh ports must not be the service's own port: a service on 80 would make
// Caddy load a host-less `:80 { … }` block into the same server that carries
// the cf/i2p/tor sites, catching everything they don't match.
func TestLayer_Enable_NeverAllocatesCaddysOwnPorts(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	require.NoError(t, l.Enable("nginxish", "nginxish", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 80, Protocol: "tcp"}}))

	block, _ := os.ReadFile(filepath.Join(root, "caddy", "conf.d-ygg", "nginxish.conf"))
	assert.NotContains(t, string(block), ":80 {")
	assert.Contains(t, string(block), ":9000 {")
	assert.Contains(t, string(block), "reverse_proxy nginxish:80")
}

func TestLayer_Enable_MultiplePorts_SeparateForwarderFiles(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{
			{Name: "web", Port: 3000, Protocol: "tcp"},
			{Name: "ssh", Port: 2222, Protocol: "tcp"},
		})
	require.NoError(t, err)

	web, err := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "gitea.forward"))
	require.NoError(t, err, "default-named forward file should exist for the first port")
	assert.Contains(t, string(web), "PORT=9000")

	ssh, err := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "gitea-ssh.forward"))
	require.NoError(t, err, "second port should get its own forward file instead of overwriting the first")
	assert.Contains(t, string(ssh), "PORT=9001")
}

// Two services declaring the same container port used to produce two socat
// forwarders binding it, the second dying with EADDRINUSE at container start.
func TestLayer_Enable_PortCollision_AllocatesNextFree(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	ports := []network.PortSelection{{Name: "web", Port: 8080, Protocol: "tcp"}}

	require.NoError(t, l.Enable("first", "first", network.ServiceInfo{}, ports))
	require.NoError(t, l.Enable("second", "second", network.ServiceInfo{}, ports))

	first, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "first.forward"))
	second, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "second.forward"))
	assert.Contains(t, string(first), "PORT=9000")
	assert.Contains(t, string(second), "PORT=9001")

	// The Caddy block must listen on the allocated port and still proxy to the
	// service's real container port.
	block, _ := os.ReadFile(filepath.Join(root, "caddy", "conf.d-ygg", "second.conf"))
	assert.Contains(t, string(block), ":9001 {")
	assert.Contains(t, string(block), "reverse_proxy second:8080")
}

// Re-enabling must not move a service: mesh peers reach it by port, and there
// is no name to look up when it changes.
func TestLayer_Enable_Reenable_KeepsAllocatedPort(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	taken := []network.PortSelection{{Name: "web", Port: 8080, Protocol: "tcp"}}
	require.NoError(t, l.Enable("first", "first", network.ServiceInfo{}, taken))
	require.NoError(t, l.Enable("second", "second", network.ServiceInfo{}, taken))

	require.NoError(t, l.Enable("second", "second", network.ServiceInfo{}, taken))
	data, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "second.forward"))
	assert.Contains(t, string(data), "PORT=9001")
}

func TestLayer_Disable_RemovesCaddyBlocks(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{
			{Name: "web", Port: 3000, Protocol: "tcp"},
			{Name: "ssh", Port: 2222, Protocol: "tcp"},
		}))
	require.NoError(t, l.Disable("gitea"))

	for _, f := range []string{"gitea.conf", "gitea-ssh.conf"} {
		_, err := os.Stat(filepath.Join(root, "caddy", "conf.d-ygg", f))
		assert.True(t, os.IsNotExist(err), "%s should be removed", f)
	}
}

func TestLayer_Disable_RemovesConfigs(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{
			{Name: "web", Port: 3000, Protocol: "tcp"},
			{Name: "ssh", Port: 2222, Protocol: "tcp"},
		}))
	require.NoError(t, l.Disable("gitea"))
	_, err := os.Stat(filepath.Join(root, "yggdrasil", "socat.d", "gitea.forward"))
	assert.True(t, os.IsNotExist(err), "default forward file should be removed")
	_, err = os.Stat(filepath.Join(root, "yggdrasil", "socat.d", "gitea-ssh.forward"))
	assert.True(t, os.IsNotExist(err), "per-port forward file should also be removed")
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	l := newForTest(t.TempDir(), noopRestart)
	assert.NoError(t, l.Disable("nonexistent"))
}
