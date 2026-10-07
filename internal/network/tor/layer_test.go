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

func TestLayer_LegacyConfDir(t *testing.T) {
	assert.Equal(t, "conf.d-tor", New("/test/repo", nil, nil).LegacyConfDir())
}

// Caddy config for tor is rendered by internal/configgen from Sites and
// written by internal/routing. Configure/Teardown here only manage torrc.d,
// the hidden-service directory, and the reload.

func TestLayer_Configure_WritesTorrcConfig(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	err := l.Configure("gitea", "gitea",
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
	assert.Contains(t, string(data), "HiddenServicePort 80 tailscale:8081")
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

func TestLayer_Teardown_RemovesConfigs(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	require.NoError(t, l.Configure("gitea", "gitea",
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))

	require.NoError(t, l.Teardown("gitea"))

	torrcPath := filepath.Join(root, "tor", "torrc.d", "gitea.conf")
	_, err := os.Stat(torrcPath)
	assert.True(t, os.IsNotExist(err), "Torrc config should be removed")
}

func TestLayer_Teardown_Idempotent(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	err := l.Teardown("nonexistent")
	assert.NoError(t, err, "Teardown should be idempotent")
}

// A root-owned bind mount is the one failure this reliably hits, and "mkdir:
// permission denied" from inside enable told nobody what to do about it.
func TestLayer_Configure_UnwritableKeyDirExplainsTheFix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tor"), 0o750))
	// What Docker leaves behind when it creates the bind-mount target itself:
	// present, but not writable by us.
	require.NoError(t, os.Mkdir(filepath.Join(root, "tor", "hidden_service"), 0o500))

	err := newForTest(root, noopReload).Configure("gitea", "gitea",
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sudo chown")
}

// The site address is the real .onion: it is a hash of a key tor generates,
// so it cannot be templated from a service name, and nothing rewrites the Host
// header on the way in — unlike i2pd's hostoverride.
func TestLayer_Sites_UseTheRealOnion(t *testing.T) {
	l := newForTest(t.TempDir(), noopReload)
	sites, err := l.Sites("gitea", "gitea", []network.PortSelection{
		{Name: "22", Port: 22, Listen: 22, Protocol: "tcp"},
		{Name: "default", Port: 3000, Protocol: "tcp"},
	})
	require.NoError(t, err)
	require.Len(t, sites, 1, "one onion, one site")
	// On the onion-only listener, not :80 — which also serves the cf and i2p
	// vhosts, reachable by a Tor client that sends their Host header.
	assert.Equal(t, "http://giteaonionaddressxxxxxxxxxxxx.onion:8081", sites[0].Address)
	assert.Equal(t, 3000, sites[0].Port, "the first HTTP port")
	assert.Equal(t, "", sites[0].PortName, "one file per service")

	_, err = l.Sites("ssh", "ssh", []network.PortSelection{{Name: "22", Port: 22, Listen: 22}})
	assert.ErrorContains(t, err, "no HTTP port")
}

// A port with its own listen port is not HTTP — an ssh port declared 22:22
// must reach the container directly, because putting Caddy in that path would
// break it.
func TestLayer_Configure_ExplicitListenPortsBypassCaddy(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	require.NoError(t, l.Configure("forgejo", "forgejo",
		[]network.PortSelection{
			{Name: "default", Port: 3000, Protocol: "tcp"},
			{Name: "22", Port: 22, Listen: 22, Protocol: "tcp"},
		}))

	data, err := os.ReadFile(filepath.Join(root, "tor", "torrc.d", "forgejo.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "HiddenServicePort 80 tailscale:8081", "HTTP via Caddy")
	assert.Contains(t, string(data), "HiddenServicePort 22 forgejo:22", "ssh direct")
}
