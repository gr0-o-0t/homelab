package diagnostics_test

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/diagnostics"
)

// writeCoreCompose lays out the on-disk shape RunPortChecks expects:
// <dir>/core/docker-compose.yml.
func writeCoreCompose(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "core"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "core", "docker-compose.yml"), []byte(body), 0o644))
	return dir
}

// occupy binds a loopback port and returns it, held until the test ends —
// standing in for the host i2pd that made this check necessary.
func occupy(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	return port
}

func TestRunPortChecks_ReportsOccupiedPort(t *testing.T) {
	port := occupy(t)
	dir := writeCoreCompose(t, `
services:
  i2p:
    container_name: i2p
    ports:
      - "127.0.0.1:${I2P_CONSOLE_PORT:-`+port+`}:7070"
`)

	// dc is nil, so the check cannot prove who holds the port and warns
	// instead of failing — but it still names the port and the container.
	g := diagnostics.RunPortChecks(dir, nil, nil, nil)
	require.Len(t, g.Results, 1)
	assert.Equal(t, diagnostics.StatusWarn, g.Results[0].Status)
	assert.Equal(t, "i2p", g.Results[0].Name)
	assert.Contains(t, g.Results[0].Message, "127.0.0.1:"+port+"/tcp")
}

func TestRunPortChecks_EnvOverrideMovesThePortOffTheCollision(t *testing.T) {
	taken := occupy(t)
	free, err := strconv.Atoi(taken)
	require.NoError(t, err)

	dir := writeCoreCompose(t, `
services:
  i2p:
    container_name: i2p
    ports:
      - "127.0.0.1:${I2P_CONSOLE_PORT:-`+taken+`}:7070"
`)

	// The default collides; the override does not. Substituting exactly as
	// compose does is the whole point — checking the default here would keep
	// crying collision after the user moved the port.
	env := map[string]string{"I2P_CONSOLE_PORT": strconv.Itoa(free + 1)}
	g := diagnostics.RunPortChecks(dir, env, nil, nil)
	require.Len(t, g.Results, 1)
	assert.Equal(t, diagnostics.StatusPass, g.Results[0].Status)
}

func TestRunPortChecks_SkipsInactiveProfiles(t *testing.T) {
	port := occupy(t)
	body := `
services:
  i2p:
    container_name: i2p
    profiles: ["i2p"]
    ports:
      - "127.0.0.1:` + port + `:7070"
`
	dir := writeCoreCompose(t, body)

	// Extension disabled → container never starts → not a collision.
	g := diagnostics.RunPortChecks(dir, nil, nil, nil)
	require.Len(t, g.Results, 1)
	assert.Equal(t, diagnostics.StatusPass, g.Results[0].Status)

	// Extension enabled → the same port is now a real problem.
	g = diagnostics.RunPortChecks(dir, nil, []string{"i2p"}, nil)
	require.Len(t, g.Results, 1)
	assert.Equal(t, diagnostics.StatusWarn, g.Results[0].Status)
}

func TestRunPortChecks_IgnoresUnpublishedAndUnpredictablePorts(t *testing.T) {
	port := occupy(t)
	dir := writeCoreCompose(t, `
services:
  a:
    container_name: a
    ports:
      - "`+port+`"
  b:
    container_name: b
    ports:
      - "8000-8010:8000-8010"
  c:
    container_name: c
    ports:
      - "${UNSET_NO_DEFAULT}:7070"
`)

	// A bare port gets a random host port, a range is not parsed, and an
	// unresolved variable has no port to test. None of them can be checked, so
	// none of them may be reported.
	g := diagnostics.RunPortChecks(dir, nil, nil, nil)
	require.Len(t, g.Results, 1)
	assert.Equal(t, diagnostics.StatusPass, g.Results[0].Status)
}

func TestRunPortChecks_MatchesUDPSeparatelyFromTCP(t *testing.T) {
	port := occupy(t) // TCP only
	dir := writeCoreCompose(t, `
services:
  i2p:
    container_name: i2p
    ports:
      - "127.0.0.1:`+port+`:45678/udp"
`)

	// The UDP port of the same number is free, so the TCP listener is not a
	// collision for it.
	g := diagnostics.RunPortChecks(dir, nil, nil, nil)
	require.Len(t, g.Results, 1)
	assert.Equal(t, diagnostics.StatusPass, g.Results[0].Status)
}

func TestRunPortChecksForFiles_ScopesToTheFilesGiven(t *testing.T) {
	port := occupy(t)
	dir := t.TempDir()

	collides := filepath.Join(dir, "collides.yml")
	require.NoError(t, os.WriteFile(collides, []byte(`
services:
  i2p:
    container_name: i2p
    ports:
      - "127.0.0.1:`+port+`:7070"
`), 0o644))

	quiet := filepath.Join(dir, "quiet.yml")
	require.NoError(t, os.WriteFile(quiet, []byte(`
services:
  jellyfin:
    container_name: jellyfin
`), 0o644))

	// `homelab up jellyfin` must not report i2p's collision — it is not
	// starting i2p, so the warning would be noise it cannot act on.
	g := diagnostics.RunPortChecksForFiles([]string{quiet}, nil, nil, nil)
	require.Len(t, g.Results, 1)
	assert.Equal(t, diagnostics.StatusPass, g.Results[0].Status)

	g = diagnostics.RunPortChecksForFiles([]string{collides}, nil, nil, nil)
	require.Len(t, g.Results, 1)
	assert.Equal(t, diagnostics.StatusWarn, g.Results[0].Status)
}
