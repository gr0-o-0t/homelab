package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/desired"
)

func putFile(t *testing.T, root, svc, rel, content string) {
	t.Helper()
	rootCmd.SetIn(strings.NewReader(content))
	t.Cleanup(func() { rootCmd.SetIn(nil) })
	runCLI(t, root, "files", "put", svc, rel)
}

// An edited config.yaml that adds a shared database is rolled out the way
// `homelab up` does: the database is provisioned before the deploy.
func TestFilesPut_ConfigEditProvisionsBeforeDeploy(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)
	recordService(root, "alpha", desired.Running)

	var provisioned []string
	old := provisionDBs
	provisionDBs = func(_ context.Context, r, name string) error {
		cfg, err := config.Load(config.ServiceConfigFile(r, name))
		require.NoError(t, err)
		dbs, err := cfg.ServiceDatabases()
		require.NoError(t, err)
		require.Len(t, dbs, 1, "the edited config.yaml is what gets provisioned")
		provisioned = append(provisioned, name)
		assert.Zero(t, calls(f, "ServiceCreate"), "provisioned before the deploy")
		return nil
	}
	t.Cleanup(func() { provisionDBs = old })

	putFile(t, root, "alpha", "config.yaml",
		"databases:\n  - postgres:\n      database: alpha\n      user: alpha\n      env:\n        host: DB_HOST\n")

	assert.Equal(t, []string{"alpha"}, provisioned)
	assert.NotEmpty(t, alphaServices(t, f), "deployed after provisioning")
	assert.Equal(t, desired.Running, loadDesired(t, root).Services["alpha"])
}

// A stopped service's file is saved but the service is not started.
func TestFilesPut_StoppedServiceNotStarted(t *testing.T) {
	f := fakeSwarm(t)
	root := webFixture(t)
	recordService(root, "alpha", desired.Stopped)

	edited := "services:\n  web:\n    image: nginx:1\n    hostname: edited\n"
	putFile(t, root, "alpha", "docker-compose.yml", edited)

	b, err := os.ReadFile(filepath.Join(root, "services", "alpha", "docker-compose.yml"))
	require.NoError(t, err)
	assert.Equal(t, edited, string(b), "saved")
	assert.Zero(t, calls(f, "ServiceCreate"), "not started")
	assert.Equal(t, desired.Stopped, loadDesired(t, root).Services["alpha"])
}

// tarEntry is one entry of a tar built by makeTar; typ defaults to a
// regular file.
type tarEntry struct {
	name, body, link string
	typ              byte
}

func makeTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typ, Linkname: e.link}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(h))
		if h.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// taashTar is the taash fixture folder as `homelab add <dir>` sends it,
// compose replaced when compose is not empty.
func taashTar(t *testing.T, compose string) []byte {
	t.Helper()
	dir := taashDir(t)
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join(dir, f))
		require.NoError(t, err)
		return string(b)
	}
	if compose == "" {
		compose = read("docker-compose.yml")
	}
	return makeTar(t,
		tarEntry{name: "config.yaml", body: read("config.yaml")},
		tarEntry{name: "conf/", typ: tar.TypeDir},
		tarEntry{name: "conf/app.conf", body: "x=1\n"},
		tarEntry{name: "docker-compose.yml", body: compose})
}

// installTar runs `files install svc` on the leader with data on stdin.
func installTar(t *testing.T, root, svc string, data []byte) (stdout, stderr string, err error) {
	t.Helper()
	resetFlags()
	t.Cleanup(resetFlags)
	var out, errb bytes.Buffer
	rootCmd.SetIn(bytes.NewReader(data))
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errb)
	t.Cleanup(func() { rootCmd.SetIn(nil); rootCmd.SetOut(nil); rootCmd.SetErr(nil) })
	rootCmd.SetArgs([]string{"--config-dir", root, "files", "install", svc})
	err = rootCmd.Execute()
	return out.String(), errb.String(), err
}

// assertNoInstall checks svc is not installed and no staging dir is left.
func assertNoInstall(t *testing.T, root, svc string) {
	t.Helper()
	assert.NoDirExists(t, filepath.Join(root, "services", svc))
	left, err := filepath.Glob(filepath.Join(root, "services", ".install-*"))
	require.NoError(t, err)
	assert.Empty(t, left, "staging dir removed")
}

func TestFilesInstall_Installs(t *testing.T) {
	fakeSwarm(t)
	root := desiredFixture(t)

	out, _, err := installTar(t, root, "taash", taashTar(t, ""))
	require.NoError(t, err)
	assert.Contains(t, out, "installed taash: run homelab setup taash (if it has secrets), homelab up taash, homelab enable taash")
	for _, f := range []string{"docker-compose.yml", "config.yaml", "conf/app.conf"} {
		assert.FileExists(t, filepath.Join(root, "services", "taash", f))
	}
	b, err := os.ReadFile(filepath.Join(root, "services", "taash", "conf", "app.conf"))
	require.NoError(t, err)
	assert.Equal(t, "x=1\n", string(b))
	left, err := filepath.Glob(filepath.Join(root, "services", ".install-*"))
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestFilesInstall_RefusesExisting(t *testing.T) {
	fakeSwarm(t)
	root := desiredFixture(t)

	_, _, err := installTar(t, root, "alpha", taashTar(t, ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.NoFileExists(t, filepath.Join(root, "services", "alpha", "config.yaml"), "alpha left as it was")
}

func TestFilesInstall_BadComposeLeavesNothing(t *testing.T) {
	fakeSwarm(t)
	root := desiredFixture(t)

	_, _, err := installTar(t, root, "taash", taashTar(t, "services: [\n"))
	require.Error(t, err)
	assertNoInstall(t, root, "taash")
}

func TestFilesInstall_TarTraversalRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    tarEntry
	}{
		{"dot-dot", tarEntry{name: "../evil", body: "x"}},
		{"nested dot-dot", tarEntry{name: "conf/../../evil", body: "x"}},
		{"absolute", tarEntry{name: "/tmp/evil", body: "x"}},
		{"symlink", tarEntry{name: "passwd", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		{"hard link", tarEntry{name: "passwd", typ: tar.TypeLink, link: "config.yaml"}},
		{"dotfile", tarEntry{name: ".env", body: "x"}},
		{"exposure.yaml", tarEntry{name: "exposure.yaml", body: "layers: [cf]\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeSwarm(t)
			root := desiredFixture(t)
			dir := taashDir(t)
			read := func(f string) string {
				b, err := os.ReadFile(filepath.Join(dir, f))
				require.NoError(t, err)
				return string(b)
			}
			data := makeTar(t,
				tarEntry{name: "config.yaml", body: read("config.yaml")},
				tarEntry{name: "docker-compose.yml", body: read("docker-compose.yml")},
				tc.e)

			_, _, err := installTar(t, root, "taash", data)
			require.Error(t, err)
			assertNoInstall(t, root, "taash")
			assert.NoFileExists(t, filepath.Join(root, "services", "evil"))
			assert.NoFileExists(t, filepath.Join(root, "evil"))
		})
	}
}

func TestFilesInstall_WarnsQualifiedLocalImage(t *testing.T) {
	fakeSwarm(t)
	root := desiredFixture(t)
	compose := "services:\n  taash:\n    image: ghcr.io/me/taash:1\n    pull_policy: never\n" +
		"  web:\n    image: nginx:1\n"

	_, stderr, err := installTar(t, root, "taash", taashTar(t, compose))
	require.NoError(t, err)
	assert.Contains(t, stderr, "warning: local image ghcr.io/me/taash:1 has a namespace or registry host; a registry image of that name would replace it")
	assert.NotContains(t, stderr, "nginx", "a pulled image is not warned about")

	// The fixture's own image (taash-server:current) is docker.io/library/…: no warning.
	root2 := desiredFixture(t)
	_, stderr, err = installTar(t, root2, "taash", taashTar(t, ""))
	require.NoError(t, err)
	assert.NotContains(t, stderr, "warning: local image")
}
