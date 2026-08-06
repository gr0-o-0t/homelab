package i2p

import (
	"encoding/binary"
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
	assert.Equal(t, "i2p", l.Name())
	assert.Equal(t, "I2P router + eepsite proxy", l.Label())
	assert.Equal(t, "i2p", l.ContainerName())
}

func TestLayer_InterfaceImplementation(t *testing.T) {
	var l network.NetworkLayer = New("/test/repo", nil, nil)
	assert.NotNil(t, l)
}

func TestLayer_CaddyConfigDir(t *testing.T) {
	l := New("/test/repo", nil, nil)
	dir := l.CaddyConfigDir("/home/user/.config/homelab")
	assert.Equal(t, "/home/user/.config/homelab/caddy/conf.d-i2p", dir)
}

// Caddy config writing/removal for i2p is owned entirely by
// internal/configgen now (see cmd/enable.go, cmd/disable.go). Enable/Disable
// here only manage tunnels.conf and the reload.

func TestLayer_Enable_WritesTunnelConfig(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	l := newForTest(root, noopReload)

	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	require.NoError(t, err)

	tunPath := filepath.Join(root, "i2p", "tunnels.conf")
	data, err := os.ReadFile(tunPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "[gitea]")
	// "tailscale", never "caddy": Caddy shares the tailscale container's
	// network namespace, so Docker DNS has no "caddy" record and i2pd's tunnel
	// would fail to resolve its upstream.
	assert.Contains(t, string(data), "host = tailscale")
	assert.NotContains(t, string(data), "host = caddy")
	assert.Contains(t, string(data), "port = 80")
	// Namespaced under the home subdomain, byte-identical to the Caddy site
	// address configgen generates — the two must agree or every request 404s.
	assert.Contains(t, string(data), "hostoverride = gitea.i2p",
		"no HOME_SUBDOMAIN in the test env, so it falls back to the bare name")
}

// Enabling the same tunnel twice must succeed, not error — this is exactly
// the sequence `homelab i2p enable <svc>` followed by its own suggested next
// step `homelab enable <svc> --i2p` produces, and previously crashed because
// the two commands maintained separately-diverged copies of this logic.
func TestLayer_Enable_DuplicateTunnel_IsIdempotent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	l := newForTest(root, noopReload)

	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))

	err := l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}})
	assert.NoError(t, err)

	tunnels, err := l.ParseTunnels()
	require.NoError(t, err)
	assert.Len(t, tunnels, 1, "the tunnel section should not be duplicated")
}

func TestLayer_Disable_RemovesConfigs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	l := newForTest(root, noopReload)

	require.NoError(t, l.Enable("gitea", "gitea", network.ServiceInfo{},
		[]network.PortSelection{{Name: "web", Port: 3000, Protocol: "tcp"}}))

	require.NoError(t, l.Disable("gitea"))

	tunPath := filepath.Join(root, "i2p", "tunnels.conf")
	data, err := os.ReadFile(tunPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "[gitea]")
}

func TestLayer_Disable_Idempotent(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)
	err := l.Disable("nonexistent")
	assert.NoError(t, err)
}

func TestLayer_ParseTunnels(t *testing.T) {
	root := t.TempDir()
	l := newForTest(root, noopReload)

	// Write a test tunnels.conf
	tunPath := filepath.Join(root, "i2p", "tunnels.conf")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	content := `[gitea]
type = http
host = caddy
port = 80
hostoverride = gitea.i2p
keys = gitea.dat
`
	require.NoError(t, os.WriteFile(tunPath, []byte(content), 0o600))

	tunnels, err := l.ParseTunnels()
	require.NoError(t, err)
	require.Len(t, tunnels, 1)
	assert.Equal(t, "gitea", tunnels[0].Name)
	assert.Equal(t, "caddy", tunnels[0].Host)
	assert.Equal(t, "80", tunnels[0].Port)
	assert.Equal(t, "gitea.i2p", tunnels[0].HostOverride)
}

func TestLayer_SectionRange(t *testing.T) {
	lines := []string{
		"",
		"[gitea]",
		"type = http",
		"host = caddy",
		"",
		"[other]",
		"type = http",
	}
	start, end, found := sectionRange(lines, "gitea")
	assert.True(t, found)
	assert.Equal(t, 1, start)
	assert.Equal(t, 5, end) // includes trailing blank
}

// ── Addressing ────────────────────────────────────────────────────────────────

// The b32 is base32(sha256(destination)), and the destination is the prefix of
// the key file whose length the spec pins at 387 + certificate length. Getting
// that parse wrong yields a confidently wrong address, so it is checked
// against a synthesised key file rather than a live router.
func TestParseDestination_UsesCertificateLength(t *testing.T) {
	// 384 bytes of keys, a null certificate (type 0, length 0), then private
	// key material that must not be included.
	null := make([]byte, 384+3+64)
	dest, err := parseDestination(null, "svc")
	require.NoError(t, err)
	assert.Len(t, dest, 387, "a null certificate makes the destination 387 bytes")

	// A key certificate carries a payload, and the spec is explicit that the
	// length at offsets 385-386 must be read, not assumed.
	keyCert := make([]byte, 384+3+4+64)
	keyCert[384] = 5 // certificate type: KEY
	binary.BigEndian.PutUint16(keyCert[385:387], 4)
	dest, err = parseDestination(keyCert, "svc")
	require.NoError(t, err)
	assert.Len(t, dest, 391)

	_, err = parseDestination(make([]byte, 100), "svc")
	assert.Error(t, err, "a file too short to hold a destination is not an address")

	truncated := make([]byte, 384+3)
	binary.BigEndian.PutUint16(truncated[385:387], 4)
	_, err = parseDestination(truncated, "svc")
	assert.Error(t, err, "certificate length beyond EOF must not slice out of range")
}

// The .i2p host is the Host header i2pd sets for Caddy, not a resolvable name —
// I2P has no global DNS. It may be listed, but never as the sole address and
// never without saying so.
func TestServiceAddresses_I2PNameIsAnAliasNotTheAddress(t *testing.T) {
	l := newForTest(t.TempDir(), noopReload)
	addrs := l.ServiceAddresses("gitea", nil)

	require.NotEmpty(t, addrs)
	last := addrs[len(addrs)-1]
	assert.Equal(t, "http://gitea.i2p", last.URL)
	assert.NotEmpty(t, last.Note, "the .i2p name must carry its caveat")
}

// The tunnel host and the Caddy site address are produced by one function, so
// a configured home subdomain has to show up in tunnels.conf as a literal —
// i2pd does no environment expansion.
func TestLayer_AppendTunnel_NamespacesUnderHomeSubdomain(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	l := &Layer{repoRoot: root, reloadHook: noopReload,
		envFn: func() map[string]string { return map[string]string{"HOME_SUBDOMAIN": "leno"} }}

	require.NoError(t, l.AppendTunnel("searxng", 8080))

	data, err := os.ReadFile(filepath.Join(root, "i2p", "tunnels.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "hostoverride = searxng.leno.i2p")
	assert.NotContains(t, string(data), "{$HOME_SUBDOMAIN}", "i2pd expands nothing")
}

// A tunnel whose host no longer matches what Caddy serves must be rewritten,
// not skipped as "already configured" — that leaves i2pd stamping a Host
// header no site block matches, which looks configured and 404s.
func TestLayer_AppendTunnel_RewritesStaleHost(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "i2p"), 0o750))
	bare := &Layer{repoRoot: root, reloadHook: noopReload}
	require.NoError(t, bare.AppendTunnel("searxng", 8080))

	moved := &Layer{repoRoot: root, reloadHook: noopReload,
		envFn: func() map[string]string { return map[string]string{"HOME_SUBDOMAIN": "leno"} }}
	require.NoError(t, moved.AppendTunnel("searxng", 8080))

	data, err := os.ReadFile(filepath.Join(root, "i2p", "tunnels.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "hostoverride = searxng.leno.i2p")
	assert.NotContains(t, string(data), "hostoverride = searxng.i2p\n")

	tunnels, err := moved.ParseTunnels()
	require.NoError(t, err)
	assert.Len(t, tunnels, 1, "the stale section must be replaced, not duplicated")
}
