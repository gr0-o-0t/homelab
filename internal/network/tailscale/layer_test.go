package tailscale

import (
	"testing"

	"github.com/groot/homelab/internal/network"
	"github.com/stretchr/testify/assert"
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
