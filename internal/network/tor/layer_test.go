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
	l := New("/test/repo", nil, nil)
	assert.Equal(t, "tor", l.Name())
	assert.Equal(t, "Tor onion service proxy", l.Label())
	assert.Equal(t, "tor", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil, nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil, nil)
	dir := l.CaddyConfigDir("/home/user/.config/homelab")
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-tor", dir)
}

// Caddy config writing/removal for tor is owned entirely by
// internal/configgen now (see cmd/enable.go, cmd/disable.go). Enable/Disable
// here only manage torrc.d, the hidden-service directory, and the reload.

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
	// HTTP goes to Caddy, not to the service container. Pointing tor straight
	// at the service — which this used to do — meant onion traffic never
	// reached Caddy, so the generated site blocks did nothing and any service
	// with a caddy.routes.conf path fan-out was broken over Tor.
	assert.Contains(t, string(data), "HiddenServicePort 80 tailscale:80")
	assert.NotContains(t, string(data), "HiddenServicePort 80 gitea:3000")

	// The per-service key directory must NOT be pre-created: tor makes it
	// itself at mode 0700 and refuses to start if it finds one more
	// permissive, which is exactly what pre-creating it at 0777 produced.
	hsDir := filepath.Join(root, "tor", "hidden_service", "gitea")
	_, err = os.Stat(hsDir)
	assert.True(t, os.IsNotExist(err), "tor owns its per-service key directory")

	// Its parent, the bind-mount target, does have to exist and be writable
	// before the container starts — otherwise Docker creates it as root.
	fi, err := os.Stat(filepath.Join(root, "tor", "hidden_service"))
	require.NoError(t, err, "the bind-mount parent should be created")
	assert.True(t, fi.IsDir())
}

func TestLayer_Disable_RemovesConfigs(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))

	require.NoError(t, l.Disable("gitea"))

	torrcPath := filepath.Join(root, "tor", "torrc.d", "gitea.conf")
	_, err := os.Stat(torrcPath)
	assert.True(t, os.IsNotExist(err), "Torrc config should be removed")
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	err := l.Disable("nonexistent")
	assert.NoError(t, err, "Disable should be idempotent")
}

// A root-owned bind mount is the one failure this reliably hits, and "mkdir:
// permission denied" from inside enable told nobody what to do about it.
func TestLayer_Enable_UnwritableKeyDirExplainsTheFix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tor"), 0o750))
	// What Docker leaves behind when it creates the bind-mount target itself:
	// present, but not writable by us.
	require.NoError(t, os.Mkdir(filepath.Join(root, "tor", "hidden_service"), 0o500))

	err := newForTest(root, noopReload).Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sudo chown")
}

// Tor owns its Caddy config for the same reason ygg does: the address is a
// hash of a key tor generates, so it cannot be templated from a service name.
// Nothing rewrites the Host header on the way in — unlike i2pd's hostoverride
// — so the site address has to be the real .onion.
func TestLayer_Enable_WritesCaddyBlockForTheRealOnion(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "default", Port: 3000, Protocol: "tcp"}}))

	block, err := os.ReadFile(filepath.Join(root, "caddy", "conf.d-tor", "gitea.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(block), "http://giteaonionaddressxxxxxxxxxxxx.onion {")
	assert.Contains(t, string(block), "reverse_proxy gitea:3000")
	assert.NotContains(t, string(block), "gitea.onion", "the templated name is not an address")
}

// A port with its own listen port is not HTTP — an ssh port declared 22:22
// must reach the container directly, because putting Caddy in that path would
// break it.
func TestLayer_Enable_ExplicitListenPortsBypassCaddy(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	require.NoError(t, l.Enable("forgejo", "forgejo", network.ServiceInfo{},
		[]network.PortSelection{
			{Name: "default", Port: 3000, Protocol: "tcp"},
			{Name: "22", Port: 22, Listen: 22, Protocol: "tcp"},
		}))

	data, err := os.ReadFile(filepath.Join(root, "tor", "torrc.d", "forgejo.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "HiddenServicePort 80 tailscale:80", "HTTP via Caddy")
	assert.Contains(t, string(data), "HiddenServicePort 22 forgejo:22", "ssh direct")
}

func TestLayer_Disable_RemovesCaddyBlock(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)
	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "default", Port: 3000, Protocol: "tcp"}}))

	require.NoError(t, l.Disable("gitea"))
	_, err := os.Stat(filepath.Join(root, "caddy", "conf.d-tor", "gitea.conf"))
	assert.True(t, os.IsNotExist(err), "a block for an address nothing answers on")
}
