package run_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/run"
)

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

func TestCommander_RunToAndRunFrom(t *testing.T) {
	var out bytes.Buffer
	c := &run.Commander{Stdout: &out, Stderr: &out}
	require.NoError(t, c.RunFrom(strings.NewReader("piped\n"), "cat"))
	assert.Equal(t, "piped\n", out.String())

	var to bytes.Buffer
	require.NoError(t, c.RunTo(&to, "echo", "streamed"))
	assert.Equal(t, "streamed\n", to.String())
}

func TestDockerNetworkExists_NonexistentNetwork(t *testing.T) {
	exists, err := run.DockerNetworkExists("nonexistent-network-12345")
	assert.NoError(t, err)
	assert.False(t, exists, "should return false for nonexistent network")
}
