package cf

import (
	"testing"

	"github.com/groot/homelab/internal/network"
	"github.com/stretchr/testify/assert"
)

func noopReload() error { return nil }

func TestLayer_Identity(t *testing.T) {
	l := New("/test/repo", nil, nil)
	assert.Equal(t, "cf", l.Name())
	assert.Equal(t, "Cloudflare Tunnel", l.Label())
	assert.Equal(t, "cloudflared", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil, nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil, nil)
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-cf", l.CaddyConfigDir("/home/user/.config/homelab"))
}

// Caddy config writing/removal for cf is owned entirely by internal/configgen
// now (see cmd/enable.go, cmd/disable.go) — Enable/Disable here are no-ops
// kept for interface conformance and registry/status/logs use.
func TestLayer_Enable_IsNoop(t *testing.T) {
	l := newForTest(t.TempDir(), noopReload)
	assert.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))
}

func TestLayer_Disable_IsNoop(t *testing.T) {
	l := newForTest(t.TempDir(), noopReload)
	assert.NoError(t, l.Disable("nonexistent"))
}
