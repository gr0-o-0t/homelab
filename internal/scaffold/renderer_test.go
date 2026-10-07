package scaffold_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/scaffold"
)

var testData = scaffold.ServiceData{
	Name:      "myapp",
	Container: "myapp-server",
	Port:      "3000",
}

// ── Render ────────────────────────────────────────────────────────────────────

func TestRender_ExpectedPaths(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.RelPath
	}
	assert.ElementsMatch(t, []string{
		"services/myapp/docker-compose.yml",
		"services/myapp/config.yaml",
	}, paths, "routes are generated from ports:, so no caddy file is scaffolded")
}

func TestRender_DockerCompose_ContainsServiceName(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	compose := findFile(t, files, "services/myapp/docker-compose.yml")
	assert.Contains(t, compose, "myapp-server", "container name should appear in compose file")
	assert.Contains(t, compose, "home-services", "should join the shared network")
}

func TestRender_ConfigYAML_ContainsScaffoldComments(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)

	cfg := findFile(t, files, "services/myapp/config.yaml")
	assert.Contains(t, cfg, "vars:")
	assert.Contains(t, cfg, "secrets:")
}

// The scaffolded declaration must parse as a plain container port. It used to
// be `web:<port>`, which the grammar reads as SUBDOMAIN "web": every new
// service was routed at web.<home>.<domain> instead of its own name.
func TestRender_DeclaresBarePort(t *testing.T) {
	root := t.TempDir()
	files, err := scaffold.Render(testData)
	require.NoError(t, err)
	require.NoError(t, scaffold.Write(root, files))

	info, err := configgen.LoadServiceInfo(root, "myapp")
	require.NoError(t, err)
	ports, err := configgen.ResolvePorts(info.Ports, nil)
	require.NoError(t, err)
	require.Len(t, ports, 1)
	assert.Equal(t, 3000, ports[0].Port)
	assert.Empty(t, ports[0].Subdomain)
	assert.Equal(t, "myapp", configgen.SiteHost(info, ""))
}

// Generated routes proxy to <service>:<port>; a container named differently
// must still answer to the service name.
func TestRender_ComposeAliasesServiceName(t *testing.T) {
	files, err := scaffold.Render(testData)
	require.NoError(t, err)
	assert.Contains(t, findFile(t, files, "services/myapp/docker-compose.yml"), "aliases:\n          - myapp\n")

	same, err := scaffold.Render(scaffold.ServiceData{Name: "app", Container: "app", Port: "80"})
	require.NoError(t, err)
	assert.NotContains(t, findFile(t, same, "services/app/docker-compose.yml"), "aliases:")
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
