package scaffold_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/scaffold"
)

var testData = scaffold.ServiceData{
	Name:      "myapp",
	Container: "myapp-server",
	Port:      "3000",
}

// ── Render ────────────────────────────────────────────────────────────────────

func TestRender_ProducesFourFiles(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)
	require.Len(t, files, 4, "expected docker-compose, caddy.conf, caddy.cf.conf, config.yaml")
}

func TestRender_ExpectedPaths(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.RelPath
	}
	assert.Contains(t, paths, "services/myapp/docker-compose.yml")
	assert.Contains(t, paths, "services/myapp/caddy.conf")
	assert.Contains(t, paths, "services/myapp/caddy.cf.conf")
	assert.Contains(t, paths, "services/myapp/config.yaml")
}

func TestRender_DockerCompose_ContainsServiceName(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	compose := findFile(t, files, "services/myapp/docker-compose.yml")
	assert.Contains(t, compose, "myapp-server", "container name should appear in compose file")
	assert.Contains(t, compose, "home-services", "should join the shared network")
}

func TestRender_PrivateCaddyConf(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	conf := findFile(t, files, "services/myapp/caddy.conf")
	assert.Contains(t, conf, "myapp.{$HOME_SUBDOMAIN}.{$DOMAIN}",
		"private caddy.conf should use HOME_SUBDOMAIN")
	assert.Contains(t, conf, "reverse_proxy myapp-server:3000")
	assert.Contains(t, conf, "import wildcard_tls")
}

func TestRender_PublicCaddyConf(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	conf := findFile(t, files, "services/myapp/caddy.cf.conf")
	assert.Contains(t, conf, "myapp.{$PUB_SUBDOMAIN}.{$DOMAIN}",
		"public caddy.cf.conf should use PUB_SUBDOMAIN")
	assert.Contains(t, conf, "reverse_proxy myapp-server:3000")
	assert.Contains(t, conf, "import wildcard_tls")
}

func TestRender_ConfigYAML_ContainsScaffoldComments(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	cfg := findFile(t, files, "services/myapp/config.yaml")
	assert.Contains(t, cfg, "vars:")
	assert.Contains(t, cfg, "secrets:")
}

func TestRender_PortSubstitution(t *testing.T) {
	data := scaffold.ServiceData{Name: "porttest", Container: "porttest", Port: "9999"}
	files, err := scaffold.Render(data)
	require.NoError(t, err)

	conf := findFile(t, files, "services/porttest/caddy.conf")
	assert.Contains(t, conf, ":9999")
}

func TestRender_PrivateAndPublicDiffer(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	private := findFile(t, files, "services/myapp/caddy.conf")
	public := findFile(t, files, "services/myapp/caddy.cf.conf")
	assert.NotEqual(t, private, public,
		"private and public Caddy configs should use different subdomain variables")
	assert.True(t, strings.Contains(private, "HOME_SUBDOMAIN") && strings.Contains(public, "PUB_SUBDOMAIN"))
}

// ── Write ─────────────────────────────────────────────────────────────────────

func TestWrite_CreatesAllFiles(t *testing.T) {
	dir := t.TempDir()
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	require.NoError(t, scaffold.Write(dir, files))

	for _, f := range files {
		path := filepath.Join(dir, f.RelPath)
		info, err := os.Stat(path)
		require.NoError(t, err, "file should exist: %s", f.RelPath)
		assert.Greater(t, info.Size(), int64(0), "file should not be empty: %s", f.RelPath)
	}
}

func TestWrite_ContentMatchesRender(t *testing.T) {
	dir := t.TempDir()
	files, err := scaffold.Render(testData)
	require.NoError(t, err)
	require.NoError(t, scaffold.Write(dir, files))

	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f.RelPath))
		require.NoError(t, err)
		assert.Equal(t, f.Content, string(data), "written content should match rendered content for %s", f.RelPath)
	}
}

func TestWrite_ErrorIfServiceDirExists(t *testing.T) {
	dir := t.TempDir()
	// Pre-create the service directory
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "services", "myapp"), 0o755))

	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	err = scaffold.Write(dir, files)
	assert.Error(t, err, "should error when service directory already exists")
	assert.Contains(t, err.Error(), "already exists")
}

func TestWrite_NoopOnEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	assert.NoError(t, scaffold.Write(dir, nil))
	assert.NoError(t, scaffold.Write(dir, []scaffold.File{}))
}

// ── helpers ───────────────────────────────────────────────────────────────────

func findFile(t *testing.T, files []scaffold.File, relPath string) string {
	t.Helper()
	for _, f := range files {
		if f.RelPath == relPath {
			return f.Content
		}
	}
	t.Fatalf("file not found in render output: %s", relPath)
	return ""
}
