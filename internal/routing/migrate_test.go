package routing_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/routing"
)

// legacy writes a service's blocks for one layer the way enable did before
// caddy/sites: one file per port in the layer's own conf dir.
func legacy(t *testing.T, root string, l network.NetworkLayer, svc, name string, ports []string) []string {
	t.Helper()
	e, err := configgen.Resolve(root, svc, name, ports)
	require.NoError(t, err)
	blocks, err := configgen.Render(l, e)
	require.NoError(t, err)
	var paths []string
	for _, b := range blocks {
		path := filepath.Join(root, "caddy", l.LegacyConfDir(), configgen.PortFileName(svc, b.PortName)+".conf")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(b.Content), 0o600))
		paths = append(paths, path)
	}
	return paths
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// legacyTree is an install from before caddy/sites:
//
//   - app:   one port, tailnet only;
//   - multi: three ports, enabled with --name=dev --ports=default,admin on
//     ts + cf + i2p;
//   - mesh:  tailnet + ygg, with an allocated mesh port;
//   - onion: tor only.
func legacyTree(t *testing.T) (root string, legacyFiles []string) {
	root = t.TempDir()
	reg := layers.New(root, nil, nil)
	get := func(n string) network.NetworkLayer { l, _ := reg.Get(n); return l }

	writeSvc(t, root, "app", map[string]string{"config.yaml": "ports:\n  - 3000\n"})
	legacyFiles = append(legacyFiles, legacy(t, root, get("ts"), "app", "", nil)...)

	writeSvc(t, root, "multi", map[string]string{"config.yaml": "ports:\n  - 3000\n  - admin:9000\n  - metrics:9100\n"})
	for _, l := range []string{"ts", "cf", "i2p"} {
		legacyFiles = append(legacyFiles, legacy(t, root, get(l), "multi", "dev", []string{"default", "admin"})...)
	}

	writeSvc(t, root, "mesh", map[string]string{"config.yaml": "ports:\n  - 8080\n"})
	legacyFiles = append(legacyFiles, legacy(t, root, get("ts"), "mesh", "", nil)...)
	yggBlock := filepath.Join(root, "caddy", "conf.d-ygg", "mesh.conf")
	writeFile(t, yggBlock, "# Yggdrasil: mesh → reachable at http://[<node address>]:9004\n:9004 {\n\treverse_proxy mesh:8080\n}\n")
	writeFile(t, filepath.Join(root, "yggdrasil", "socat.d", "mesh.forward"), "PORT=9004\nTARGET=tailscale:9004\n")
	legacyFiles = append(legacyFiles, yggBlock)

	writeSvc(t, root, "onion", map[string]string{"config.yaml": "ports:\n  - 80\n"})
	torBlock := filepath.Join(root, "caddy", "conf.d-tor", "onion.conf")
	writeFile(t, torBlock, "# Tor: onion\nhttp://abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz23.onion:8081 {\n\treverse_proxy onion:80\n}\n")
	legacyFiles = append(legacyFiles, torBlock)

	// Not ours to touch: a prefix sibling's file and daemon-side config.
	writeSvc(t, root, "app-extra", map[string]string{"config.yaml": "ports:\n  - 4000\n"})
	writeFile(t, filepath.Join(root, "tor", "torrc.d", "onion.conf"), "HiddenServiceDir x\n")
	return root, legacyFiles
}

func TestMigrate_LegacyTree(t *testing.T) {
	root, legacyFiles := legacyTree(t)
	reg := layers.New(root, nil, nil)

	pending, err := routing.Pending(root, reg)
	require.NoError(t, err)
	assert.Equal(t, []string{"app", "mesh", "multi", "onion"}, pending)

	done, err := routing.Migrate(root, reg)
	require.NoError(t, err)
	require.Len(t, done, 4)

	state := func(svc string) exposure.State {
		st, err := exposure.Load(root, svc)
		require.NoError(t, err)
		return st
	}
	assert.Equal(t, exposure.State{Layers: []string{"ts"}}, state("app"))
	assert.Equal(t, exposure.State{Layers: []string{"ts", "cf", "i2p"}, Name: "dev", Ports: []string{"admin", "default"}}, state("multi"))
	assert.Equal(t, exposure.State{Layers: []string{"ts", "ygg"}, YggPorts: map[string]int{"default": 9004}}, state("mesh"))
	assert.Equal(t, exposure.State{Layers: []string{"tor"}}, state("onion"))
	assert.Contains(t, read(t, exposure.Path(root, "app")), "Do not edit by hand")

	// The sites file is what enable would render from that state now.
	for _, svc := range []string{"app", "multi", "mesh", "onion"} {
		assert.FileExists(t, configgen.SitesFile(root, svc))
	}
	multi := read(t, configgen.SitesFile(root, "multi"))
	for _, want := range []string{
		"dev.{$HOME_SUBDOMAIN}.{$DOMAIN} {", "admin.{$HOME_SUBDOMAIN}.{$DOMAIN} {",
		"http://dev.{$DOMAIN} {", "http://admin.{$DOMAIN} {",
		"http://dev.{$HOME_SUBDOMAIN}.i2p {", "http://admin.{$HOME_SUBDOMAIN}.i2p {",
	} {
		assert.Contains(t, multi, want)
	}
	assert.NotContains(t, multi, "metrics", "metrics was not selected")
	assert.Contains(t, read(t, configgen.SitesFile(root, "mesh")), ":9004 {")
	assert.Contains(t, read(t, configgen.SitesFile(root, "onion")),
		"http://abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz23.onion:8081 {")

	for _, f := range legacyFiles {
		assert.NoFileExists(t, f)
	}
	assert.NoFileExists(t, configgen.SitesFile(root, "app-extra"))
	assert.NoFileExists(t, exposure.Path(root, "app-extra"))
	assert.FileExists(t, filepath.Join(root, "tor", "torrc.d", "onion.conf"))
	assert.FileExists(t, filepath.Join(root, "yggdrasil", "socat.d", "mesh.forward"))

	// The migrated sites file renders identically from the stored state.
	before := read(t, configgen.SitesFile(root, "multi"))
	require.NoError(t, routing.Sync(root, reg, "multi"))
	assert.Equal(t, before, read(t, configgen.SitesFile(root, "multi")))
}

// A second run finds nothing to do and changes nothing.
func TestMigrate_Idempotent(t *testing.T) {
	root, _ := legacyTree(t)
	reg := layers.New(root, nil, nil)
	_, err := routing.Migrate(root, reg)
	require.NoError(t, err)
	sites := read(t, configgen.SitesFile(root, "multi"))
	state := read(t, exposure.Path(root, "multi"))

	pending, err := routing.Pending(root, reg)
	require.NoError(t, err)
	assert.Empty(t, pending)
	done, err := routing.Migrate(root, reg)
	require.NoError(t, err)
	assert.Empty(t, done)
	assert.Equal(t, sites, read(t, configgen.SitesFile(root, "multi")))
	assert.Equal(t, state, read(t, exposure.Path(root, "multi")))
}

// A run that wrote exposure.yaml but could not render keeps the legacy files,
// and the next run resumes from the state it wrote.
func TestMigrate_FailureKeepsLegacyAndResumes(t *testing.T) {
	root := t.TempDir()
	reg := layers.New(root, nil, nil)
	writeSvc(t, root, "hidden", map[string]string{"config.yaml": "ports:\n  - 80\n"})
	torBlock := filepath.Join(root, "caddy", "conf.d-tor", "hidden.conf")
	// No onion anywhere: not in the block, no hostname file, no tor running.
	writeFile(t, torBlock, "# Tor: hidden\nhttp://:8081 {\n\treverse_proxy hidden:80\n}\n")

	_, err := routing.Migrate(root, reg)
	require.ErrorContains(t, err, "tor has not published an address")
	assert.FileExists(t, torBlock, "nothing is removed before the new file exists")
	assert.NoFileExists(t, configgen.SitesFile(root, "hidden"))
	st, err := exposure.Load(root, "hidden")
	require.NoError(t, err)
	assert.Equal(t, []string{"tor"}, st.Layers, "the inferred state is kept for the next run")

	writeFile(t, filepath.Join(root, "tor", "hidden_service", "hidden", "hostname"), "xyz.onion\n")
	done, err := routing.Migrate(root, reg)
	require.NoError(t, err)
	require.Len(t, done, 1)
	assert.NoFileExists(t, torBlock)
	assert.Contains(t, read(t, configgen.SitesFile(root, "hidden")), "http://xyz.onion:8081 {")
}

// Layers that disagree about a port cannot share one --ports selection without
// exposing more or less than before: refuse and touch nothing. ygg lacking a
// port is fine — it simply has no mesh port for it.
func TestMigrate_PerLayerPortsMismatch(t *testing.T) {
	root := t.TempDir()
	reg := layers.New(root, nil, nil)
	ts, _ := reg.Get("ts")
	cfl, _ := reg.Get("cf")
	writeSvc(t, root, "gitea", map[string]string{"config.yaml": "ports:\n  - 3000\n  - admin:9000\n"})
	tsFiles := legacy(t, root, ts, "gitea", "", nil)
	legacy(t, root, cfl, "gitea", "", []string{"default"})

	_, err := routing.Migrate(root, reg)
	require.ErrorContains(t, err, `port "admin" is routed on ts but not on cf`)
	assert.NoFileExists(t, exposure.Path(root, "gitea"))
	assert.NoFileExists(t, configgen.SitesFile(root, "gitea"))
	for _, f := range tsFiles {
		assert.FileExists(t, f)
	}

	// Dropping the extra file makes them agree; ygg forwarding only one of
	// the two ports is representable.
	require.NoError(t, os.Remove(filepath.Join(root, "caddy", "conf.d", "gitea-admin.conf")))
	writeFile(t, filepath.Join(root, "caddy", "conf.d-ygg", "gitea.conf"), ":9002 {\n\treverse_proxy gitea:3000\n}\n")
	_, err = routing.Migrate(root, reg)
	require.NoError(t, err)
	st, err := exposure.Load(root, "gitea")
	require.NoError(t, err)
	assert.Equal(t, exposure.State{Layers: []string{"ts", "cf", "ygg"}, Ports: []string{"default"}, YggPorts: map[string]int{"default": 9002}}, st)
	assert.Contains(t, read(t, configgen.SitesFile(root, "gitea")), ":9002 {")
}
