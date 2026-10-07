package cf

import (
	"testing"

	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestLayer_LegacyConfDir(t *testing.T) {
	assert.Equal(t, "conf.d-cf", New("/test/repo", nil, nil).LegacyConfDir())
}

// cf is nothing but Caddy routing: no per-service daemon state.
func TestLayer_HasNoDaemonSide(t *testing.T) {
	_, ok := any(New("/test/repo", nil, nil)).(network.Configurer)
	assert.False(t, ok)
}

// The advertised hostname is the one the cf site block answers on, so --name and
// declared subdomains show up instead of the bare service name.
func TestLayer_ServiceAddresses_UsesSiteHost(t *testing.T) {
	root := t.TempDir()
	l := New(root, nil, nil)
	env := map[string]string{"DOMAIN": "example.com"}
	assert.Equal(t, "https://gitea.example.com", l.ServiceAddresses("gitea", env)[0].URL)

	require.NoError(t, exposure.Save(root, "gitea", exposure.State{Layers: []string{"cf"}, Name: "git"}))
	assert.Equal(t, "https://git.example.com", l.ServiceAddresses("gitea", env)[0].URL)
}
