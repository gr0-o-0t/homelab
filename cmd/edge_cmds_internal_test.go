package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `logs` with no service and `caddy logs` read the leader agent's container:
// Caddy runs inside it.
func TestLogs_NoServiceIsTheAgent(t *testing.T) {
	root := t.TempDir()
	got := stubServiceLogs(t)

	code, _, _ := runLogs(t, "--config-dir", root, "logs", "-f", "-n", "20", "--until", "1h")
	require.Zero(t, code)
	code, _, _ = runLogs(t, "--config-dir", root, "caddy", "logs")
	require.Zero(t, code)
	assert.Equal(t, [][]string{
		{"logs", "--follow", "--tail", "20", "--until", "1h", "homelab-agent"},
		{"logs", "--follow", "homelab-agent"},
	}, *got)

	code, _, _ = runLogs(t, "--config-dir", root, "logs", "-c", "server")
	assert.NotZero(t, code, "-c needs a service")
}

// `caddy status` asks the agent's admin socket; with no agent it reports
// Caddy down rather than failing.
func TestCaddyStatus_AgentDown(t *testing.T) {
	t.Setenv("HOMELAB_CADDY_ADMIN", filepath.Join(t.TempDir(), "none.sock"))
	code, _, _ := runLogs(t, "--config-dir", t.TempDir(), "caddy", "status")
	assert.Zero(t, code)
}

func composeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeSvc(t, root, "web", map[string]string{
		"docker-compose.yml": "services:\n" +
			"  app:\n    image: nginx:${TAG:-1.27}\n    volumes: [data:/data]\n" +
			"  worker:\n    image: nginx:${TAG:-1.27}\n" +
			"  cache:\n    image: redis:7\n" +
			"volumes:\n  data:\n    name: web_data\n  scratch: {}\n",
	})
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"), []byte("vars: {}\n"), 0o600))
	return root
}

// `config` and `images` load the compose file as a deploy does — no docker
// compose involved.
func TestConfigAndImages_FromTheComposeFile(t *testing.T) {
	root := composeFixture(t)

	code, out, _ := runLogs(t, "--config-dir", root, "config", "web")
	require.Zero(t, code)
	assert.Contains(t, out, "image: nginx:1.27")
	assert.Contains(t, out, "name: web")

	code, out, _ = runLogs(t, "--config-dir", root, "images", "web")
	require.Zero(t, code)
	assert.Contains(t, out, "CONTAINER")
	assert.Regexp(t, `cache\s+redis:7`, out)
	assert.Regexp(t, `worker\s+nginx:1\.27`, out)

	code, out, _ = runLogs(t, "--config-dir", root, "images", "-q", "web")
	require.Zero(t, code)
	assert.Equal(t, "nginx:1.27\nredis:7\n", out, "each reference once")

	code, _, _ = runLogs(t, "--config-dir", root, "config")
	assert.NotZero(t, code, "a service is required")
}

// prune removes the volumes the stack deploys with — named or <svc>_<key> —
// and the images it runs.
func TestPlanPrune_VolumesAndImages(t *testing.T) {
	root := composeFixture(t)
	setConfigDir(t, root)
	plan, err := planPrune(root, "web")
	require.NoError(t, err)
	assert.Equal(t, []string{"web_data", "web_scratch"}, plan.volumes)
	assert.Equal(t, []string{"nginx:1.27", "redis:7"}, plan.images, "by compose key, each once")
}
