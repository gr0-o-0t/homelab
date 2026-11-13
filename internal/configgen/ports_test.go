package configgen

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var portsEnv = map[string]string{"HOME_SUBDOMAIN": "home", "DOMAIN": "example.com"}

func writePortsService(t *testing.T, root, svc, cfg, exposureYAML string) {
	t.Helper()
	dir := filepath.Join(root, "services", svc)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644))
	if exposureYAML != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "exposure.yaml"), []byte(exposureYAML), 0o644))
	}
}

func TestPortStatuses_GiteaHTTPAndRaw(t *testing.T) {
	root := t.TempDir()
	writePortsService(t, root, "gitea", "ports:\n  - 3000\n  - 2222:2222/tcp\n", "layers: [ts]\n")

	got := PortStatuses(root, "gitea", portsEnv)
	require.Len(t, got, 2)
	assert.Equal(t, PortStatus{
		Name: "2222", Label: "2222/tcp", Raw: true, Port: 2222, Listen: 2222, Protocols: []string{"tcp"},
		Target: "gitea.home.example.com:2222/tcp", Enabled: true,
	}, got[0])
	assert.Equal(t, PortStatus{
		Name: "default", Label: "https", Port: 3000,
		Target: "https://gitea.home.example.com", Enabled: true,
	}, got[1])
	assert.Equal(t, []string{"2222/tcp", "https"}, EnabledPortLabels(got))
}

func TestPortStatuses_PortsFilterAndLayers(t *testing.T) {
	root := t.TempDir()
	// --ports=default: the raw port is declared but not enabled.
	writePortsService(t, root, "gitea", "ports:\n  - 3000\n  - 2222:2222/tcp\n",
		"layers: [ts]\nname: git\nports: [default]\n")
	got := PortStatuses(root, "gitea", portsEnv)
	require.Len(t, got, 2)
	assert.False(t, got[0].Enabled, "2222 is outside the --ports selection")
	assert.Equal(t, "git.home.example.com:2222/tcp", got[0].Target, "where it would answer, under --name")
	assert.True(t, got[1].Enabled)
	assert.Equal(t, "https://git.home.example.com", got[1].Target)
	assert.Equal(t, []string{"https"}, EnabledPortLabels(got))

	// Cloudflare only: HTTP is public, a raw port is tailnet-only.
	writePortsService(t, root, "gitea", "ports:\n  - 3000\n  - 2222:2222/tcp\n", "layers: [cf]\n")
	got = PortStatuses(root, "gitea", portsEnv)
	assert.False(t, got[0].Enabled, "raw ports exist on the tailnet layer only")
	assert.True(t, got[1].Enabled)
	assert.Equal(t, "https://gitea.example.com", got[1].Target)
}

func TestPortStatuses_VaultwardenSubdomain(t *testing.T) {
	root := t.TempDir()
	writePortsService(t, root, "vaultwarden", "ports:\n  - vault:80\n", "layers: [ts, cf]\n")
	got := PortStatuses(root, "vaultwarden", portsEnv)
	require.Len(t, got, 1)
	assert.Equal(t, PortStatus{
		Name: "vault", Label: "vault", Port: 80,
		Target: "https://vault.home.example.com", Enabled: true,
	}, got[0])
}

func TestPortStatuses_RoutesService(t *testing.T) {
	root := t.TempDir()
	writePortsService(t, root, "vaultwarden", "ports:\n  - vault:80\n", "layers: [ts]\n")
	require.NoError(t, os.WriteFile(RoutesFile(root, "vaultwarden"), []byte("reverse_proxy vaultwarden:80\n"), 0o644))
	got := PortStatuses(root, "vaultwarden", portsEnv)
	require.Len(t, got, 1)
	assert.Equal(t, "vault", got[0].Label)
	assert.Equal(t, "https://vault.home.example.com", got[0].Target)
	assert.True(t, got[0].Enabled)
}

func TestPortStatuses_NotExposed(t *testing.T) {
	root := t.TempDir()
	writePortsService(t, root, "grafana", "ports:\n  - 3000\n", "")
	got := PortStatuses(root, "grafana", portsEnv)
	require.Len(t, got, 1)
	assert.False(t, got[0].Enabled)
	assert.Equal(t, "https://grafana.home.example.com", got[0].Target, "where enable would put it")
	assert.Empty(t, EnabledPortLabels(got))
}

func TestPortStatuses_UDPAndNoEnv(t *testing.T) {
	root := t.TempDir()
	writePortsService(t, root, "dns", "ports:\n  - 53:53\n  - 8080/udp\n", "layers: [ts]\n")
	got := PortStatuses(root, "dns", nil)
	require.Len(t, got, 2)
	assert.Equal(t, "53/tcp+udp", got[0].Label)
	assert.Empty(t, got[0].Target, "no DOMAIN: no address")
	assert.True(t, got[0].Enabled)
	assert.Equal(t, "8080/udp", got[1].Label)
	assert.False(t, got[1].Enabled, "udp without a listen port is never routed")
	assert.NotEmpty(t, got[1].Note)

	assert.Nil(t, PortStatuses(root, "missing", portsEnv), "no config.yaml: no ports")
}
