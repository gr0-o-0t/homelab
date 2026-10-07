package caddy

// White-box test — same package so we can access newForTest and the unexported
// reloadFn field to bypass Docker/Caddy without changing the production API.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/cf"
	"github.com/groot/homelab/internal/network/tailscale"
)

// noopReload is injected in every test to skip Docker exec.
func noopReload() error { return nil }

// writeSvc lays out an installed service with the given files.
func writeSvc(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "services", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	for f, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600))
	}
}

const routesBody = "handle /api/* {\n\treverse_proxy svc-api:8000\n}\n\nhandle {\n\treverse_proxy svc:80\n}\n"

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// enabled records a service as on the given layers, the way enable leaves it.
func enabled(t *testing.T, root, svc string, st exposure.State) {
	t.Helper()
	require.NoError(t, exposure.Save(root, svc, st))
}

func sites(root, svc string) string { return configgen.SitesFile(root, svc) }

// ── ReloadService ─────────────────────────────────────────────────────────────

// A reload must re-render the layers that are on without switching on the
// ones that are off.
func TestReloadService_Routes_OnlyTouchesActiveLayers(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "appflowy", map[string]string{
		"config.yaml": "ports:\n  - 80\n", configgen.RoutesFileName: routesBody,
	})
	enabled(t, root, "appflowy", exposure.State{Layers: []string{"ts"}})
	require.NoError(t, os.MkdirAll(configgen.SitesDir(root), 0o750))
	require.NoError(t, os.WriteFile(sites(root, "appflowy"), []byte("stale\n"), 0o600))

	require.NoError(t, newForTest(root, noopReload).ReloadService("appflowy"))

	got := read(t, sites(root, "appflowy"))
	assert.Contains(t, got, "reverse_proxy svc-api:8000", "active layer should be re-rendered")
	assert.NotContains(t, got, "http://appflowy.{$DOMAIN}", "reload must not enable a layer the user left off")
}

// Port-driven services — most of the catalog — keep the --name they were
// enabled with, and pick up a changed upstream port.
func TestReloadService_Ports_RegeneratesAndKeepsName(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	enabled(t, root, "gitea", exposure.State{Layers: []string{"ts", "cf"}, Name: "git"})

	reloads := 0
	m := newForTest(root, func() error { reloads++; return nil })
	require.NoError(t, m.ReloadService("gitea"))

	got := read(t, sites(root, "gitea"))
	assert.Contains(t, got, "git.{$HOME_SUBDOMAIN}.{$DOMAIN} {")
	assert.Contains(t, got, "http://git.{$DOMAIN} {")
	assert.Equal(t, 2, strings.Count(got, "reverse_proxy gitea:3000"))
	assert.Equal(t, 1, reloads, "the file is rewritten, then Caddy reloads once")
}

func TestReloadService_NothingActive(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	err := newForTest(root, noopReload).ReloadService("gitea")
	assert.ErrorContains(t, err, "no active routes",
		"reloading a service with no enabled layer should say so, not silently succeed")
	assert.NoFileExists(t, sites(root, "gitea"))
}

// ── Rollback ──────────────────────────────────────────────────────────────────

// A config Caddy rejects must not stay on disk: Caddy would refuse to start at
// its next restart, taking every route down.
func TestReloadService_RollsBackWhenValidationFails(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	enabled(t, root, "gitea", exposure.State{Layers: []string{"ts"}})
	require.NoError(t, os.MkdirAll(configgen.SitesDir(root), 0o750))
	require.NoError(t, os.WriteFile(sites(root, "gitea"), []byte("old\n"), 0o600))
	other := sites(root, "other")
	require.NoError(t, os.WriteFile(other, []byte("# kept\n"), 0o600))

	m := newForTest(root, func() error { return fmt.Errorf("%w: bad", ErrInvalidConfig) })
	err := m.ReloadService("gitea")
	require.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "rolled back")

	assert.Equal(t, "old\n", read(t, sites(root, "gitea")))
	assert.Equal(t, "# kept\n", read(t, other), "unrelated config is untouched")
}

// Only a validation failure rolls back; any other reload error (Caddy down)
// leaves the change in place for the next start.
func TestReloadOrRestore_KeepsChangeOnOtherErrors(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	enabled(t, root, "gitea", exposure.State{Layers: []string{"ts"}})

	m := newForTest(root, func() error { return errors.New("caddy not running") })
	require.Error(t, m.ReloadService("gitea"))
	assert.Contains(t, read(t, sites(root, "gitea")), "reverse_proxy gitea:3000")
}

// The snapshot covers exactly a service's two routing files, including their
// absence.
func TestSnapshot_RestoresBothFilesAndAbsence(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	enabled(t, root, "gitea", exposure.State{Layers: []string{"ts"}})
	state := read(t, exposure.Path(root, "gitea"))
	m := newForTest(root, noopReload)

	snap, err := m.Snapshot("gitea")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configgen.SitesDir(root), 0o750))
	require.NoError(t, os.WriteFile(sites(root, "gitea"), []byte("added\n"), 0o600))
	require.NoError(t, os.WriteFile(exposure.Path(root, "gitea"), []byte("changed\n"), 0o600))

	require.NoError(t, snap.Restore())
	assert.NoFileExists(t, sites(root, "gitea"))
	assert.Equal(t, state, read(t, exposure.Path(root, "gitea")))
}

// ── Enable / Disable ──────────────────────────────────────────────────────────

// fakeDaemon is a layer with daemon-side state, recording what it is asked.
type fakeDaemon struct {
	*tailscale.Layer
	calls []string
}

func (f *fakeDaemon) Name() string { return "fake" }
func (f *fakeDaemon) Flag() string { return "fake" }
func (f *fakeDaemon) Sites(_, host string, _ []network.PortSelection) ([]network.Site, error) {
	return []network.Site{{Address: "http://" + host + ".fake", Port: 1}}, nil
}
func (f *fakeDaemon) Configure(svc, display string, _ []network.PortSelection) error {
	f.calls = append(f.calls, "configure "+svc+" "+display)
	return nil
}
func (f *fakeDaemon) Teardown(svc string) error {
	f.calls = append(f.calls, "teardown "+svc)
	return nil
}

func testRegistry(root string) (*network.Registry, *fakeDaemon) {
	reg := network.NewRegistry()
	reg.Register(tailscale.New(root, nil, nil))
	reg.Register(cf.New(root, nil, nil))
	fake := &fakeDaemon{Layer: tailscale.New(root, nil, nil)}
	reg.Register(fake)
	return reg, fake
}

func layersOf(reg *network.Registry, names ...string) []network.NetworkLayer {
	var out []network.NetworkLayer
	for _, n := range names {
		l, _ := reg.Get(n)
		out = append(out, l)
	}
	return out
}

func strPtr(s string) *string { return &s }

// Enabling then disabling every layer leaves nothing behind: no sites file,
// and no exposure.yaml (a service on no layer has none).
func TestEnableDisable_RoundTrip(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	reg, fake := testRegistry(root)
	m := newForTest(root, noopReload)

	_, err := m.Enable(reg, "gitea", layersOf(reg, "ts", "cf", "fake"), strPtr("git"), nil)
	require.NoError(t, err)
	st, _ := exposure.Load(root, "gitea")
	assert.Equal(t, exposure.State{Layers: []string{"ts", "cf", "fake"}, Name: "git"}, st)
	got := read(t, sites(root, "gitea"))
	assert.Contains(t, got, "git.{$HOME_SUBDOMAIN}.{$DOMAIN} {")
	assert.Contains(t, got, "http://git.{$DOMAIN} {")
	assert.Contains(t, got, "http://git.fake {")

	require.NoError(t, m.Disable(reg, "gitea", layersOf(reg, "ts", "cf", "fake")))
	assert.NoFileExists(t, sites(root, "gitea"))
	assert.NoFileExists(t, exposure.Path(root, "gitea"))
	assert.Equal(t, []string{"configure gitea git", "teardown gitea"}, fake.calls)
}

// --name and --ports are remembered: enabling another layer without them keeps
// them, and every layer is rendered with them. The service name and a full
// port list mean "no override".
func TestEnable_RemembersNameAndPorts(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n  - admin:9000\n"})
	reg, _ := testRegistry(root)
	m := newForTest(root, noopReload)

	_, err := m.Enable(reg, "gitea", layersOf(reg, "ts"), strPtr("git"), []string{"default"})
	require.NoError(t, err)
	_, err = m.Enable(reg, "gitea", layersOf(reg, "cf"), nil, nil)
	require.NoError(t, err)
	st, _ := exposure.Load(root, "gitea")
	assert.Equal(t, exposure.State{Layers: []string{"ts", "cf"}, Name: "git", Ports: []string{"default"}}, st)
	assert.NotContains(t, read(t, sites(root, "gitea")), "admin")

	_, err = m.Enable(reg, "gitea", layersOf(reg, "ts"), strPtr("gitea"), []string{"admin", "default"})
	require.NoError(t, err)
	st, _ = exposure.Load(root, "gitea")
	assert.Equal(t, exposure.State{Layers: []string{"ts", "cf"}}, st)
	assert.Contains(t, read(t, sites(root, "gitea")), "http://admin.{$DOMAIN} {")
}

// When Caddy rejects the result, enable puts both files back exactly and tears
// down the daemon side of the layers it had turned on.
func TestEnable_RollsBackBothFilesOnInvalidConfig(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	reg, fake := testRegistry(root)
	_, err := newForTest(root, noopReload).Enable(reg, "gitea", layersOf(reg, "ts"), nil, nil)
	require.NoError(t, err)
	beforeSites, beforeState := read(t, sites(root, "gitea")), read(t, exposure.Path(root, "gitea"))

	m := newForTest(root, func() error { return fmt.Errorf("%w: bad", ErrInvalidConfig) })
	undone, err := m.Enable(reg, "gitea", layersOf(reg, "ts", "cf", "fake"), strPtr("git"), nil)
	require.ErrorIs(t, err, ErrInvalidConfig)
	assert.Equal(t, []string{"cf", "fake"}, undone)
	assert.Equal(t, beforeSites, read(t, sites(root, "gitea")))
	assert.Equal(t, beforeState, read(t, exposure.Path(root, "gitea")))
	assert.Equal(t, []string{"configure gitea git", "teardown gitea"}, fake.calls)
}

// A rejected disable changes nothing, daemon side included.
func TestDisable_RollsBackOnInvalidConfig(t *testing.T) {
	root := t.TempDir()
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	reg, fake := testRegistry(root)
	_, err := newForTest(root, noopReload).Enable(reg, "gitea", layersOf(reg, "ts", "fake"), nil, nil)
	require.NoError(t, err)
	beforeSites, beforeState := read(t, sites(root, "gitea")), read(t, exposure.Path(root, "gitea"))

	m := newForTest(root, func() error { return fmt.Errorf("%w: bad", ErrInvalidConfig) })
	require.ErrorIs(t, m.Disable(reg, "gitea", layersOf(reg, "fake")), ErrInvalidConfig)
	assert.Equal(t, beforeSites, read(t, sites(root, "gitea")))
	assert.Equal(t, beforeState, read(t, exposure.Path(root, "gitea")))
	assert.Equal(t, []string{"configure gitea gitea"}, fake.calls, "no teardown after a rollback")
}
