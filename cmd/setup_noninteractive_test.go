package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/secrets"
)

// fakeKeyring swaps the keyring for an in-memory one for the test.
func fakeKeyring(t *testing.T) *secrets.Manager {
	t.Helper()
	sm := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	prev := openSecrets
	openSecrets = func() (*secrets.Manager, error) { return sm, nil }
	t.Cleanup(func() { openSecrets = prev })
	return sm
}

const setupTestConfig = `vars:
  WEB_PORT:
    value: "8080"
    required: true
    description: "Port the app listens on"
  THEME:
    value: ""
    required: false
secrets:
  API_KEY:
    required: true
    description: "Upstream API key"
  SESSION:
    required: true
    generate: password
`

func setupTestDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	svc := filepath.Join(root, "services", "web")
	require.NoError(t, os.MkdirAll(svc, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(svc, "docker-compose.yml"), []byte("services: {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(svc, "config.yaml"), []byte(setupTestConfig), 0o600))
	return root
}

func runSetupCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(resetFlags)
	resetFlags()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetIn(strings.NewReader(stdin))
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetIn(nil) })
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return out.String(), err
}

func TestSetupJSON_Service(t *testing.T) {
	root := setupTestDir(t)
	sm := fakeKeyring(t)
	require.NoError(t, sm.Set("web", "API_KEY", "hunter2"))

	out, err := runSetupCmd(t, "", "--config-dir", root, "setup", "web", "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, "hunter2", "secret values must never be printed")

	var info SetupInfo
	require.NoError(t, json.Unmarshal([]byte(out), &info))
	assert.Equal(t, "web", info.Service)
	assert.Equal(t, []SetupVar{
		{Name: "THEME"},
		{Name: "WEB_PORT", Value: "8080", Required: true, Description: "Port the app listens on"},
	}, info.Vars)
	assert.Equal(t, []SetupSecret{
		{Name: "API_KEY", Required: true, Description: "Upstream API key", Set: true},
		{Name: "SESSION", Required: true, Generated: true},
	}, info.Secrets)
}

func TestSetupJSON_RootWithoutConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	fakeKeyring(t)

	out, err := runSetupCmd(t, "", "--config-dir", root, "setup", "--json")
	require.NoError(t, err)
	var info SetupInfo
	require.NoError(t, json.Unmarshal([]byte(out), &info))
	assert.Empty(t, info.Service)
	var names []string
	for _, v := range info.Vars {
		names = append(names, v.Name)
		if v.Name == "HOME_SUBDOMAIN" {
			assert.Equal(t, "home", v.Value)
			assert.Equal(t, "Subdomain prefix", v.Description)
		}
	}
	assert.Contains(t, names, "DOMAIN")
	require.NotEmpty(t, info.Secrets)
	for _, s := range info.Secrets {
		assert.False(t, s.Set, s.Name)
	}
	_, err = os.Stat(filepath.Join(root, "config.yaml"))
	assert.True(t, os.IsNotExist(err), "--json must not write anything")
}

func TestSetupSet_ServiceVarsAndSecrets(t *testing.T) {
	root := setupTestDir(t)
	sm := fakeKeyring(t)

	_, err := runSetupCmd(t, `{"API_KEY":"s3cret"}`,
		"--config-dir", root, "setup", "web", "--set", "WEB_PORT=9090", "--set", "THEME=dark", "--secrets-stdin")
	require.NoError(t, err)

	cfg, err := config.Load(config.ServiceConfigFile(root, "web"))
	require.NoError(t, err)
	assert.Equal(t, "9090", cfg.Vars["WEB_PORT"].Value)
	assert.Equal(t, "dark", cfg.Vars["THEME"].Value)
	assert.Equal(t, "Port the app listens on", cfg.Vars["WEB_PORT"].Description, "saving keeps descriptions")

	v, err := sm.Get("web", "API_KEY")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", v)
	gen, err := sm.Get("web", "SESSION")
	require.NoError(t, err)
	assert.NotEmpty(t, gen, "generated secrets are minted, as in interactive setup")
}

func TestSetupSet_RejectsUnknownAndChangesNothing(t *testing.T) {
	root := setupTestDir(t)
	sm := fakeKeyring(t)

	_, err := runSetupCmd(t, `{"API_KEY":"s3cret"}`,
		"--config-dir", root, "setup", "web", "--set", "WEB_PORT=9090", "--set", "NOPE=1", "--secrets-stdin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NOPE")
	cfg, _ := config.Load(config.ServiceConfigFile(root, "web"))
	assert.Equal(t, "8080", cfg.Vars["WEB_PORT"].Value)
	assert.False(t, sm.IsSet("web", "API_KEY"))

	_, err = runSetupCmd(t, "", "--config-dir", root, "setup", "web", "--set", "API_KEY=x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--secrets-stdin", "a secret on argv is refused")

	_, err = runSetupCmd(t, `{"BOGUS":"x"}`, "--config-dir", root, "setup", "web", "--secrets-stdin")
	require.Error(t, err)

	_, err = runSetupCmd(t, "", "--config-dir", root, "setup", "web", "--json", "--set", "WEB_PORT=1")
	require.Error(t, err)
}

func TestSetupSet_Root(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	sm := fakeKeyring(t)

	_, err := runSetupCmd(t, `{"TS_AUTHKEY":"tskey-x","CLOUDFLARE_API_TOKEN":""}`,
		"--config-dir", root, "setup", "--set", "DOMAIN=example.com", "--secrets-stdin")
	require.NoError(t, err)

	cfg, err := config.Load(filepath.Join(root, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "example.com", cfg.Vars["DOMAIN"].Value)
	assert.Equal(t, "home", cfg.Vars["HOME_SUBDOMAIN"].Value, "defaults are kept")
	v, _ := sm.Get("", "TS_AUTHKEY")
	assert.Equal(t, "tskey-x", v)
	assert.False(t, sm.IsSet("", "CLOUDFLARE_API_TOKEN"), "an empty value leaves the secret unchanged")
	_, err = os.Stat(filepath.Join(root, "core"))
	assert.NoError(t, err, "core files are installed as in interactive setup")
}
