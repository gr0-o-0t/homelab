package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/clistate"
)

// portsFixture: gitea (3000 + raw 2222/tcp) on the tailnet, vaultwarden
// (vault:80) on the tailnet and Cloudflare, grafana declared but not exposed.
func portsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"), []byte(
		"vars:\n  DOMAIN:\n    value: example.com\n  HOME_SUBDOMAIN:\n    value: home\n"), 0o600))
	compose := "services: {}\n"
	writeSvc(t, root, "gitea", map[string]string{"docker-compose.yml": compose,
		"config.yaml": "ports:\n  - 3000\n  - 2222:2222/tcp\n", "exposure.yaml": "layers: [ts]\n"})
	writeSvc(t, root, "vaultwarden", map[string]string{"docker-compose.yml": compose,
		"config.yaml": "ports:\n  - vault:80\n", "exposure.yaml": "layers: [ts, cf]\n"})
	writeSvc(t, root, "grafana", map[string]string{"docker-compose.yml": compose,
		"config.yaml": "ports:\n  - 3000\n"})
	t.Setenv("HOMELAB_CADDY_ADMIN", filepath.Join(t.TempDir(), "none.sock"))
	return root
}

func TestStatus_PortsColumn(t *testing.T) {
	root := portsFixture(t)
	out := captureStdout(t, func() { runCLI(t, root, "--json=false", "status") })
	lines := map[string]string{}
	for _, svc := range []string{"gitea", "vaultwarden", "grafana"} {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), svc+" ") {
				lines[svc] = l
				break
			}
		}
	}
	assert.Contains(t, lines["gitea"], "2222/tcp, https", out)
	assert.Contains(t, lines["vaultwarden"], "vault", out)
	assert.Contains(t, lines["grafana"], "–", "not exposed: no ports")
	assert.NotContains(t, lines["grafana"], "https")
}

func TestStatusService_PortsSection(t *testing.T) {
	root := portsFixture(t)
	env := map[string]string{"DOMAIN": "example.com", "HOME_SUBDOMAIN": "home"}

	out := captureStdout(t, func() { require.NoError(t, runServiceStatus(root, "gitea", env)) })
	assert.Contains(t, out, "Ports")
	assert.Contains(t, out, "gitea.home.example.com:2222/tcp")
	assert.Contains(t, out, "https://gitea.home.example.com")
	assert.Contains(t, out, "default")

	out = captureStdout(t, func() { require.NoError(t, runServiceStatus(root, "grafana", env)) })
	assert.Contains(t, out, "not enabled")
	assert.Contains(t, out, "https://grafana.home.example.com")
}

func TestStatusJSON_Ports(t *testing.T) {
	root := portsFixture(t)
	t.Cleanup(resetFlags)

	out := captureStdout(t, func() { runCLI(t, root, "--json", "status") })
	var st clistate.Status
	require.NoError(t, json.Unmarshal([]byte(out), &st), out)
	byName := map[string]clistate.Service{}
	for _, s := range st.Services {
		byName[s.Name] = s
	}
	gitea := byName["gitea"].Ports
	require.Len(t, gitea, 2, out)
	assert.Equal(t, "2222", gitea[0].Name)
	assert.Equal(t, "2222/tcp", gitea[0].Label)
	assert.True(t, gitea[0].Raw)
	assert.True(t, gitea[0].Enabled)
	assert.Equal(t, "https", gitea[1].Label)
	require.Len(t, byName["vaultwarden"].Ports, 1)
	assert.Equal(t, "https://vault.home.example.com", byName["vaultwarden"].Ports[0].Target)
	require.Len(t, byName["grafana"].Ports, 1)
	assert.False(t, byName["grafana"].Ports[0].Enabled)

	out = captureStdout(t, func() { runCLI(t, root, "--json", "status", "gitea") })
	var one clistate.Service
	require.NoError(t, json.Unmarshal([]byte(out), &one), out)
	require.Len(t, one.Ports, 2)
	assert.Contains(t, out, `"ports"`)
	assert.Equal(t, "gitea.home.example.com:2222/tcp", one.Ports[0].Target)
}
