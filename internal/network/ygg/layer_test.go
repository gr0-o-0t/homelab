package ygg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
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

func TestLayer_LegacyConfDir(t *testing.T) {
	assert.Equal(t, "conf.d-ygg", New("/test/repo", nil, nil).LegacyConfDir())
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

// enable does what internal/routing does for `homelab enable --ygg`: the
// daemon side, then the blocks rendered from Sites. The mesh has no naming, so
// Caddy routes by listening port and only this layer knows it. Each block is
// written to rendered/<port file>.conf for the assertions.
func enable(l *Layer, svc string, ports []network.PortSelection) error {
	if err := l.Configure(svc, svc, ports); err != nil {
		return err
	}
	blocks, err := configgen.Render(l, configgen.Exposure{Service: svc, Host: svc, Ports: ports})
	if err != nil {
		return err
	}
	dir := filepath.Join(l.repoRoot, "rendered")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, b := range blocks {
		if err := os.WriteFile(filepath.Join(dir, configgen.PortFileName(svc, b.PortName)+".conf"), []byte(b.Content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// disable mirrors the daemon half of `homelab disable --ygg`.
func disable(l *Layer, svc string) error { return l.Teardown(svc) }

func TestLayer_Enable_WritesForwarderAndCaddyBlock(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	err := enable(l, "gitea",
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)

	data, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "gitea.forward"))
	assert.Contains(t, string(data), "PORT=9000")
	// Via Caddy, not straight at the service container: "tailscale" is where
	// Caddy listens (it shares that container's network namespace).
	assert.Contains(t, string(data), "TARGET=tailscale:9000")
	assert.NotContains(t, string(data), "TARGET=gitea:3000")

	block, err := os.ReadFile(filepath.Join(root, "rendered", "gitea.conf"))
	require.NoError(t, err, "a :<mesh port> site block")
	assert.Contains(t, string(block), ":9000 {")
	assert.Contains(t, string(block), "reverse_proxy gitea:3000")
}

// Mesh ports must not be the service's own port: a service on 80 would make
// Caddy load a host-less `:80 { … }` block into the same server that carries
// the cf/i2p/tor sites, catching everything they don't match.
func TestLayer_Enable_NeverAllocatesCaddysOwnPorts(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	require.NoError(t, enable(l, "nginxish",
		[]network.PortSelection{{Name: "web", Port: 80, Protocol: "tcp"}}))

	block, _ := os.ReadFile(filepath.Join(root, "rendered", "nginxish.conf"))
	assert.NotContains(t, string(block), ":80 {")
	assert.Contains(t, string(block), ":9000 {")
	assert.Contains(t, string(block), "reverse_proxy nginxish:80")
}

func TestLayer_Enable_MultiplePorts_SeparateForwarderFiles(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	err := enable(l, "gitea",
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

	require.NoError(t, enable(l, "first", ports))
	require.NoError(t, enable(l, "second", ports))

	first, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "first.forward"))
	second, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "second.forward"))
	assert.Contains(t, string(first), "PORT=9000")
	assert.Contains(t, string(second), "PORT=9001")

	// The Caddy block must listen on the allocated port and still proxy to the
	// service's real container port.
	block, _ := os.ReadFile(filepath.Join(root, "rendered", "second.conf"))
	assert.Contains(t, string(block), ":9001 {")
	assert.Contains(t, string(block), "reverse_proxy second:8080")
}

// Re-enabling must not move a service: mesh peers reach it by port, and there
// is no name to look up when it changes.
func TestLayer_Enable_Reenable_KeepsAllocatedPort(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	taken := []network.PortSelection{{Name: "web", Port: 8080, Protocol: "tcp"}}
	require.NoError(t, enable(l, "first", taken))
	require.NoError(t, enable(l, "second", taken))

	require.NoError(t, enable(l, "second", taken))
	data, _ := os.ReadFile(filepath.Join(root, "yggdrasil", "socat.d", "second.forward"))
	assert.Contains(t, string(data), "PORT=9001")
}

func TestLayer_Disable_RemovesConfigs(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	writeGiteaPorts(t, root)
	require.NoError(t, enable(l, "gitea",
		[]network.PortSelection{
			{Name: "web", Port: 3000, Protocol: "tcp"},
			{Name: "ssh", Port: 2222, Protocol: "tcp"},
		}))
	require.NoError(t, disable(l, "gitea"))
	_, err := os.Stat(filepath.Join(root, "yggdrasil", "socat.d", "gitea.forward"))
	assert.True(t, os.IsNotExist(err), "default forward file should be removed")
	_, err = os.Stat(filepath.Join(root, "yggdrasil", "socat.d", "gitea-ssh.forward"))
	assert.True(t, os.IsNotExist(err), "per-port forward file should also be removed")
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	l := newForTest(t.TempDir(), noopRestart)
	assert.NoError(t, disable(l, "nonexistent"))
}

// writeGiteaPorts declares the ports the multi-port tests enable, since Disable
// derives the per-port file names from the declaration.
func writeGiteaPorts(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "services", "gitea")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("ports:\n  - web:3000\n  - ssh:2222\n"), 0o600))
}

// Disabling "foo" used to glob foo-*.forward / foo-*.conf, which also matched
// every file of a service called "foo-bar".
func TestLayer_Disable_LeavesPrefixSiblingAlone(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	ports := []network.PortSelection{{Name: "default", Port: 8080, Protocol: "tcp"}}
	require.NoError(t, enable(l, "foo", ports))
	require.NoError(t, enable(l, "foo-bar", ports))

	require.NoError(t, disable(l, "foo"))

	for _, f := range []string{
		filepath.Join(root, "yggdrasil", "socat.d", "foo-bar.forward"),
	} {
		_, err := os.Stat(f)
		assert.NoError(t, err, "%s belongs to foo-bar and must survive disabling foo", f)
	}
	st, err := exposure.Load(root, "foo-bar")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"default": 9001}, st.YggPorts, "foo-bar keeps its mesh port")
	_, err = os.Stat(filepath.Join(root, "yggdrasil", "socat.d", "foo.forward"))
	assert.True(t, os.IsNotExist(err))
}

// The allocation is recorded in exposure.yaml, the registry every service's
// allocation is read from; disable releases it.
func TestLayer_Allocation_RecordedInExposure(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopRestart)
	writeGiteaPorts(t, root)
	require.NoError(t, enable(l, "gitea", []network.PortSelection{
		{Name: "web", Port: 3000, Protocol: "tcp"},
		{Name: "ssh", Port: 2222, Protocol: "tcp"},
	}))
	st, err := exposure.Load(root, "gitea")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"web": 9000, "ssh": 9001}, st.YggPorts)

	// Narrowing the ports keeps the remaining port where it was and drops the
	// deselected port's forwarder.
	require.NoError(t, enable(l, "gitea", []network.PortSelection{{Name: "ssh", Port: 2222, Protocol: "tcp"}}))
	st, _ = exposure.Load(root, "gitea")
	assert.Equal(t, map[string]int{"ssh": 9001}, st.YggPorts)
	assert.NoFileExists(t, filepath.Join(root, "yggdrasil", "socat.d", "gitea.forward"))

	require.NoError(t, disable(l, "gitea"))
	assert.NoFileExists(t, exposure.Path(root, "gitea"), "nothing left to record")
}
