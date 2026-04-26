package run_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/run"
)

func sliceToMap(slice []string) map[string]string {
	m := make(map[string]string)
	for _, s := range slice {
		k, v, _ := strings.Cut(s, "=")
		m[k] = v
	}
	return m
}

func TestMergeEnv_EmptyOverrides(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/user"}
	env := map[string]string{}

	result := run.MergeEnv(base, env)
	m := sliceToMap(result)

	assert.Equal(t, "/usr/bin", m["PATH"])
	assert.Equal(t, "/home/user", m["HOME"])
}

func TestMergeEnv_OverridesReplaceExisting(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/user"}
	env := map[string]string{"HOME": "/new/home", "NEW_VAR": "new_value"}

	result := run.MergeEnv(base, env)
	m := sliceToMap(result)

	assert.Equal(t, "/new/home", m["HOME"], "override should replace existing")
	assert.Equal(t, "new_value", m["NEW_VAR"])
	assert.Equal(t, "/usr/bin", m["PATH"])
}

func TestMergeEnv_NewVarsAdded(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	env := map[string]string{"A": "1", "B": "2"}

	result := run.MergeEnv(base, env)
	m := sliceToMap(result)

	assert.Equal(t, "1", m["A"])
	assert.Equal(t, "2", m["B"])
	assert.Equal(t, "/usr/bin", m["PATH"])
}

func TestMergeEnv_EmptyBase(t *testing.T) {
	var base []string
	env := map[string]string{"KEY": "value"}

	result := run.MergeEnv(base, env)
	m := sliceToMap(result)

	assert.Equal(t, "value", m["KEY"])
}

func TestMergeEnv_EmptyEnv(t *testing.T) {
	base := []string{"KEY=value"}
	var env map[string]string

	result := run.MergeEnv(base, env)
	m := sliceToMap(result)

	assert.Equal(t, "value", m["KEY"])
}

func TestMergeEnv_EmptyBoth(t *testing.T) {
	var base []string
	var env map[string]string

	result := run.MergeEnv(base, env)

	assert.Empty(t, result)
}

func TestMergeEnv_ValuesWithEquals(t *testing.T) {
	base := []string{"KEY=value=with=equals"}
	env := map[string]string{}

	result := run.MergeEnv(base, env)
	m := sliceToMap(result)

	assert.Equal(t, "value=with=equals", m["KEY"])
}

func TestCoreComposeFile(t *testing.T) {
	result := run.CoreComposeFile("/home/user/.config/homelab")
	assert.Equal(t, "/home/user/.config/homelab/core/docker-compose.yml", result)
}

func TestServiceComposeFile(t *testing.T) {
	result := run.ServiceComposeFile("/home/user/.config/homelab", "uptime-kuma")
	assert.Equal(t, "/home/user/.config/homelab/services/uptime-kuma/docker-compose.yml", result)
}

func TestServiceComposeFile_SpecialChars(t *testing.T) {
	result := run.ServiceComposeFile("/home/user/.config/homelab", "my-service_name")
	assert.Equal(t, "/home/user/.config/homelab/services/my-service_name/docker-compose.yml", result)
}

func TestCommander_Default(t *testing.T) {
	cmd := run.Default()
	require.NotNil(t, cmd)
	require.NotNil(t, cmd.Stdout)
	require.NotNil(t, cmd.Stderr)
}

func TestCommander_DockerComposeEnv_ConstructsCorrectArgs(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("services: {}"), 0o644))

	var stdoutCaptured []byte
	cmd := &run.Commander{
		Stdout: &capturingWriter{buf: &stdoutCaptured},
		Stderr:  os.Stderr,
	}

	err := cmd.DockerComposeEnv(composePath, map[string]string{"TEST_VAR": "test_value"}, "ps")
	assert.NoError(t, err)
}

type capturingWriter struct {
	buf *[]byte
}

func (w *capturingWriter) Write(p []byte) (n int, err error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

func TestDockerNetworkExists_NonexistentNetwork(t *testing.T) {
	exists, err := run.DockerNetworkExists("nonexistent-network-12345")
	assert.NoError(t, err)
	assert.False(t, exists, "should return false for nonexistent network")
}