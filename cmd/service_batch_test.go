package cmd

// White-box tests for the new batch/group helpers and doctor utilities.
// Same package (cmd) required to access unexported functions.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
)

// setConfigDir temporarily overrides rootFlags so rootConfigFile() resolves to
// a test-controlled directory. Restores original values via t.Cleanup.
func setConfigDir(t *testing.T, dir string) {
	t.Helper()
	oldDir, oldFile := rootFlags.configDir, rootFlags.configFile
	rootFlags.configDir = dir
	rootFlags.configFile = "" // ensure configDir takes precedence
	t.Cleanup(func() {
		rootFlags.configDir = oldDir
		rootFlags.configFile = oldFile
	})
}

// makeBatchFixture creates a temp dir with two minimal services (alpha, beta).
func makeBatchFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		svcDir := filepath.Join(root, "services", name)
		require.NoError(t, os.MkdirAll(svcDir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(svcDir, "docker-compose.yml"),
			[]byte("services: {}\n"), 0o644))
	}
	return root
}

// ── resolveTargets ────────────────────────────────────────────────────────────

func TestResolveTargets_SingleArg(t *testing.T) {
	root := t.TempDir()
	names, err := resolveTargets(root, false, "", []string{"jellyfin"})
	require.NoError(t, err)
	assert.Equal(t, []string{"jellyfin"}, names)
}

func TestResolveTargets_NoArgNoFlags_Errors(t *testing.T) {
	root := t.TempDir()
	_, err := resolveTargets(root, false, "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

func TestResolveTargets_AllAndGroupMutuallyExclusive(t *testing.T) {
	root := t.TempDir()
	_, err := resolveTargets(root, true, "media", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestResolveTargets_PositionalArgWithAll_Errors(t *testing.T) {
	root := t.TempDir()
	_, err := resolveTargets(root, true, "", []string{"jellyfin"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot combine")
}

func TestResolveTargets_PositionalArgWithGroup_Errors(t *testing.T) {
	root := t.TempDir()
	_, err := resolveTargets(root, false, "media", []string{"jellyfin"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot combine")
}

func TestResolveTargets_All_ReturnsAllServices(t *testing.T) {
	root := makeBatchFixture(t)
	names, err := resolveTargets(root, true, "", nil)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, names)
}

func TestResolveTargets_All_EmptyServicesDir(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "services"), 0o755))
	names, err := resolveTargets(root, true, "", nil)
	require.NoError(t, err)
	assert.Empty(t, names)
}

func TestResolveTargets_All_MissingServicesDir(t *testing.T) {
	root := t.TempDir() // no services/ subdirectory
	names, err := resolveTargets(root, true, "", nil)
	require.NoError(t, err)
	assert.Empty(t, names)
}

func TestResolveTargets_Group_ValidGroup(t *testing.T) {
	root := makeBatchFixture(t)
	setConfigDir(t, root)

	cfg := &config.Config{
		Vars:   map[string]config.VarEntry{"DOMAIN": {Value: "test.com"}},
		Groups: map[string][]string{"media": {"alpha", "beta"}},
	}
	require.NoError(t, config.Save(filepath.Join(root, "config.yaml"), cfg))

	names, err := resolveTargets(root, false, "media", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta"}, names)
}

func TestResolveTargets_Group_UnknownGroup(t *testing.T) {
	root := makeBatchFixture(t)
	setConfigDir(t, root)

	cfg := &config.Config{
		Groups: map[string][]string{"media": {"alpha"}},
	}
	require.NoError(t, config.Save(filepath.Join(root, "config.yaml"), cfg))

	_, err := resolveTargets(root, false, "nonexistent", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), "media") // error lists known groups
}

func TestResolveTargets_Group_NoGroupsDefined(t *testing.T) {
	root := makeBatchFixture(t)
	setConfigDir(t, root)

	cfg := &config.Config{
		Vars: map[string]config.VarEntry{"DOMAIN": {Value: "test.com"}},
	}
	require.NoError(t, config.Save(filepath.Join(root, "config.yaml"), cfg))

	_, err := resolveTargets(root, false, "media", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no groups")
}

func TestResolveTargets_Group_NoConfigFile(t *testing.T) {
	root := makeBatchFixture(t)
	setConfigDir(t, root) // no config.yaml written

	_, err := resolveTargets(root, false, "media", nil)
	require.Error(t, err)
	// Missing config.yaml is treated as "no groups defined"
	assert.Contains(t, err.Error(), "no groups")
}

// ── firstOrEmpty ──────────────────────────────────────────────────────────────

func TestFirstOrEmpty_WithArg(t *testing.T) {
	assert.Equal(t, "jellyfin", firstOrEmpty([]string{"jellyfin"}))
}

func TestFirstOrEmpty_NilArgs(t *testing.T) {
	assert.Equal(t, "<service>", firstOrEmpty(nil))
}

func TestFirstOrEmpty_EmptySlice(t *testing.T) {
	assert.Equal(t, "<service>", firstOrEmpty([]string{}))
}

// ── removeBrokenSymlinks ──────────────────────────────────────────────────────

func TestRemoveBrokenSymlinks_NonexistentDirectory(t *testing.T) {
	count := removeBrokenSymlinks("/nonexistent/path/xyz", false)
	assert.Equal(t, 0, count)
}

func TestRemoveBrokenSymlinks_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	count := removeBrokenSymlinks(dir, false)
	assert.Equal(t, 0, count)
}

func TestRemoveBrokenSymlinks_ValidSymlink_NotCounted(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-file")
	require.NoError(t, os.WriteFile(target, []byte("content"), 0o644))
	link := filepath.Join(dir, "good-link.conf")
	require.NoError(t, os.Symlink(target, link))

	count := removeBrokenSymlinks(dir, true)
	assert.Equal(t, 0, count)

	_, err := os.Lstat(link)
	assert.NoError(t, err, "valid symlink must remain")
}

func TestRemoveBrokenSymlinks_BrokenLink_ReportOnly(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "broken.conf")
	require.NoError(t, os.Symlink("/absolutely-nonexistent/target", link))

	count := removeBrokenSymlinks(dir, false /* fix=false */)
	assert.Equal(t, 1, count)

	_, err := os.Lstat(link)
	assert.NoError(t, err, "link must remain when fix=false")
}

func TestRemoveBrokenSymlinks_BrokenLink_Fix(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "broken.conf")
	require.NoError(t, os.Symlink("/absolutely-nonexistent/target", link))

	count := removeBrokenSymlinks(dir, true /* fix=true */)
	assert.Equal(t, 1, count)

	_, err := os.Lstat(link)
	assert.True(t, os.IsNotExist(err), "broken symlink must be removed when fix=true")
}

func TestRemoveBrokenSymlinks_RegularFile_Ignored(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular.conf")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0o644))

	count := removeBrokenSymlinks(dir, true)
	assert.Equal(t, 0, count)

	_, err := os.Stat(file)
	assert.NoError(t, err, "regular file must remain")
}

func TestRemoveBrokenSymlinks_MixedLinks(t *testing.T) {
	dir := t.TempDir()

	target := filepath.Join(dir, "real-file")
	require.NoError(t, os.WriteFile(target, []byte("content"), 0o644))
	goodLink := filepath.Join(dir, "good.conf")
	require.NoError(t, os.Symlink(target, goodLink))

	require.NoError(t, os.Symlink("/nonexistent/a", filepath.Join(dir, "broken-a.conf")))
	require.NoError(t, os.Symlink("/nonexistent/b", filepath.Join(dir, "broken-b.conf")))

	count := removeBrokenSymlinks(dir, true)
	assert.Equal(t, 2, count, "both broken links counted and removed")

	_, err := os.Lstat(goodLink)
	assert.NoError(t, err, "valid symlink must remain")
}

// ── publicHostname ────────────────────────────────────────────────────────────

func TestPublicHostname_WithPubSubdomain(t *testing.T) {
	env := map[string]string{
		"PUB_SUBDOMAIN": "pub",
		"DOMAIN":        "example.com",
	}
	assert.Equal(t, "jellyfin.pub.example.com", publicHostname("jellyfin", env))
}

func TestPublicHostname_DefaultPubSubdomain(t *testing.T) {
	env := map[string]string{
		"DOMAIN": "example.com",
		// PUB_SUBDOMAIN not set — should default to "pub"
	}
	assert.Equal(t, "myapp.pub.example.com", publicHostname("myapp", env))
}

func TestPublicHostname_CustomPubSubdomain(t *testing.T) {
	env := map[string]string{
		"PUB_SUBDOMAIN": "open",
		"DOMAIN":        "myhomelab.io",
	}
	assert.Equal(t, "gitea.open.myhomelab.io", publicHostname("gitea", env))
}
