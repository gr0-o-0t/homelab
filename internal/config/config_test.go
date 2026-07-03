package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/99designs/keyring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/secrets"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// erroringKeyring is a keyring.Keyring that always fails, for exercising
// BuildEnv/injectDBEnv's handling of a genuine backend failure (as opposed
// to "secret not set", which Manager.Get reports as a nil error).
type erroringKeyring struct{}

func (erroringKeyring) Get(string) (keyring.Item, error) {
	return keyring.Item{}, errors.New("backend unavailable")
}
func (erroringKeyring) GetMetadata(string) (keyring.Metadata, error) {
	return keyring.Metadata{}, errors.New("backend unavailable")
}
func (erroringKeyring) Set(keyring.Item) error  { return errors.New("backend unavailable") }
func (erroringKeyring) Remove(string) error     { return errors.New("backend unavailable") }
func (erroringKeyring) Keys() ([]string, error) { return nil, errors.New("backend unavailable") }

func erroringSecretsManager() *secrets.Manager {
	return secrets.NewForTest(erroringKeyring{})
}

// ── Load ──────────────────────────────────────────────────────────────────────

func TestLoad_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
vars:
  DOMAIN:
    value: example.com
    required: true
  HOME_SUBDOMAIN:
    value: home
    required: true
secrets:
  TS_AUTHKEY:
    required: true
`)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "example.com", cfg.Vars["DOMAIN"].Value)
	assert.True(t, cfg.Vars["DOMAIN"].Required)
	assert.Equal(t, "home", cfg.Vars["HOME_SUBDOMAIN"].Value)
	assert.True(t, cfg.Secrets["TS_AUTHKEY"].Required)
}

func TestLoad_Missing(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	assert.NoError(t, err)
	assert.Nil(t, cfg, "missing file should return nil, not error")
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", ":\t:bad yaml{{")
	_, err := config.Load(path)
	assert.Error(t, err)
}

// ── Save ──────────────────────────────────────────────────────────────────────

func TestSave_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := &config.Config{
		Vars: map[string]config.VarEntry{
			"DOMAIN":         {Value: "roundtrip.io", Required: true},
			"HOME_SUBDOMAIN": {Value: "lab", Required: true},
		},
		Secrets: map[string]config.SecretEntry{
			"TS_AUTHKEY": {Required: true},
		},
	}

	require.NoError(t, config.Save(path, original))

	loaded, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, original.Vars["DOMAIN"].Value, loaded.Vars["DOMAIN"].Value)
	assert.Equal(t, original.Vars["DOMAIN"].Required, loaded.Vars["DOMAIN"].Required)
	assert.True(t, loaded.Secrets["TS_AUTHKEY"].Required)
}

func TestSave_Roundtrip_WithGroups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := &config.Config{
		Vars: map[string]config.VarEntry{
			"DOMAIN": {Value: "example.io", Required: true},
		},
		Groups: map[string][]string{
			"media": {"jellyfin", "immich"},
			"utils": {"vaultwarden"},
		},
	}

	require.NoError(t, config.Save(path, original))

	loaded, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, original.Groups, loaded.Groups)
	assert.Equal(t, []string{"jellyfin", "immich"}, loaded.Groups["media"])
	assert.Equal(t, []string{"vaultwarden"}, loaded.Groups["utils"])
}

func TestSave_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "config.yaml")
	cfg := &config.Config{Vars: map[string]config.VarEntry{"X": {Value: "1"}}}
	require.NoError(t, config.Save(path, cfg))
	_, err := os.Stat(path)
	assert.NoError(t, err)
}

// ── RootConfigFile / ServiceConfigFile ────────────────────────────────────────

func TestRootConfigFile_DefaultsToConfigDir(t *testing.T) {
	result := config.RootConfigFile("/home/user/.config/homelab", "")
	assert.Equal(t, "/home/user/.config/homelab/config.yaml", result)
}

func TestRootConfigFile_FlagOverrides(t *testing.T) {
	result := config.RootConfigFile("/home/user/.config/homelab", "/custom/path/config.yaml")
	assert.Equal(t, "/custom/path/config.yaml", result)
}

func TestServiceConfigFile(t *testing.T) {
	result := config.ServiceConfigFile("/home/user/.config/homelab", "uptime-kuma")
	assert.Equal(t, "/home/user/.config/homelab/services/uptime-kuma/config.yaml", result)
}

// ── BuildEnv ──────────────────────────────────────────────────────────────────

func TestBuildEnv_RootVars(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
vars:
  DOMAIN:
    value: example.com
    required: true
  ACME_EMAIL:
    value: admin@example.com
    required: true
`)
	env, err := config.BuildEnv(cfgPath, dir, "", nil)
	require.NoError(t, err)
	assert.Equal(t, "example.com", env["DOMAIN"])
	assert.Equal(t, "admin@example.com", env["ACME_EMAIL"])
}

func TestBuildEnv_MissingRootConfig(t *testing.T) {
	dir := t.TempDir()
	env, err := config.BuildEnv(filepath.Join(dir, "config.yaml"), dir, "", nil)
	require.NoError(t, err)
	assert.NotNil(t, env)
}

func TestBuildEnv_ServiceVarsOverrideRoot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
vars:
  DOMAIN:
    value: root.com
    required: true
  HOME_SUBDOMAIN:
    value: home
    required: true
`)
	writeFile(t, dir, "services/myapp/config.yaml", `
vars:
  DOMAIN:
    value: service-override.com
    required: false
  APP_PORT:
    value: "8080"
    required: false
`)
	env, err := config.BuildEnv(cfgPath, dir, "myapp", nil)
	require.NoError(t, err)
	assert.Equal(t, "service-override.com", env["DOMAIN"], "service var should override root")
	assert.Equal(t, "home", env["HOME_SUBDOMAIN"], "root var not overridden by service should remain")
	assert.Equal(t, "8080", env["APP_PORT"], "service-only var should be present")
}

func TestBuildEnv_EmptyVarValueSkipped(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
vars:
  DOMAIN:
    value: ""
    required: true
  ACME_EMAIL:
    value: set@example.com
    required: true
`)
	env, err := config.BuildEnv(cfgPath, dir, "", nil)
	require.NoError(t, err)
	_, hasDomain := env["DOMAIN"]
	assert.False(t, hasDomain, "empty value should not be injected into env")
	assert.Equal(t, "set@example.com", env["ACME_EMAIL"])
}

func TestBuildEnv_NoServiceConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
vars:
  DOMAIN:
    value: base.com
    required: true
`)
	env, err := config.BuildEnv(cfgPath, dir, "nonexistent-service", nil)
	require.NoError(t, err)
	assert.Equal(t, "base.com", env["DOMAIN"], "root vars still present when service config is missing")
}

// ── DefaultConfigDir ──────────────────────────────────────────────────────────

func TestDefaultConfigDir_XDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
	result := config.DefaultConfigDir()
	assert.Equal(t, "/custom/xdg/homelab", result)
}

func TestDefaultConfigDir_HomeDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	result := config.DefaultConfigDir()
	home, _ := os.UserHomeDir()
	assert.Equal(t, filepath.Join(home, ".config", "homelab"), result)
}

// ── PortEntries ────────────────────────────────────────────────────────────

func TestPortEntries_OldFormat(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `
ports:
  web:
    port: 8080
    protocol: tcp
`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Ports)
	assert.Equal(t, 8080, cfg.Ports["web"].Port)
	assert.Equal(t, "tcp", cfg.Ports["web"].Protocol)
}

func TestPortEntries_NewFormat_Named(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `ports:
  - web:8080
`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Ports)
	assert.Equal(t, 8080, cfg.Ports["web"].Port)
	assert.Equal(t, "tcp", cfg.Ports["web"].Protocol)
}

func TestPortEntries_NewFormat_Unnamed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `ports:
  - 8080
`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Ports)
	assert.Equal(t, 8080, cfg.Ports["default"].Port)
}

func TestPortEntries_NewFormat_Mapped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `ports:
  - 8080:9090
`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Ports)
	// Mapped port stored under host port as key, container port as value
	assert.Equal(t, 9090, cfg.Ports["8080"].Port)
}

func TestPortEntries_NewFormat_Multiple(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `ports:
  - web:8080
  - ssh:22
  - 3000
`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Ports)
	assert.Equal(t, 8080, cfg.Ports["web"].Port)
	assert.Equal(t, 22, cfg.Ports["ssh"].Port)
	assert.Equal(t, 3000, cfg.Ports["default"].Port)
}

func TestPortEntries_EmptyList(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `ports:`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Nil(t, cfg.Ports)
}

func TestPortEntries_SaveRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := &config.Config{
		Ports: config.PortEntries{
			"web": {Port: 8080, Protocol: "tcp"},
			"ssh": {Port: 22, Protocol: "tcp"},
		},
	}
	require.NoError(t, config.Save(path, original))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, original.Ports["web"], loaded.Ports["web"])
	assert.Equal(t, original.Ports["ssh"], loaded.Ports["ssh"])
}

func TestPortEntries_DuplicateDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `ports:
  - 3000
  - 4000
`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	assert.ErrorContains(t, err, "at most one unnamed/mapped port allowed")
	assert.Nil(t, cfg)
}

func TestPortEntries_DefaultPlusMapped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `ports:
  - 3000
  - 22:22
`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Ports)
	assert.Equal(t, 3000, cfg.Ports["default"].Port)
	assert.Equal(t, 22, cfg.Ports["22"].Port)
}

// ── ExtensionLabel / AllExtensions canonical-name consistency ────────────────

func TestExtensionLabel_UsesCanonicalNamesFromAllExtensions(t *testing.T) {
	// Every name AllExtensions() yields (what callers like `ext list` iterate)
	// must have a real label in ExtensionLabel, not fall through to the
	// identity default — that's exactly how "ygg" silently regressed to
	// showing "ygg" as its own label after the alias was renamed from
	// "yggdrasil" without updating ExtensionLabel's switch to match.
	for _, name := range config.AllExtensions() {
		label := config.ExtensionLabel(name)
		assert.NotEqual(t, name, label, "ExtensionLabel(%q) fell through to the identity default", name)
	}
}

func TestExtensionLabel_ResolvesLegacyYggdrasilAlias(t *testing.T) {
	assert.Equal(t, "ygg", config.ResolveExtension("yggdrasil"))
	assert.Equal(t, config.ExtensionLabel("ygg"), config.ExtensionLabel(config.ResolveExtension("yggdrasil")))
}

// ── DSN templating: single-pass replace ──────────────────────────────────────

func TestBuildEnv_DSNPlaceholderCollision_SinglePassReplace(t *testing.T) {
	// A user/database value that happens to contain another placeholder's
	// literal token used to get corrupted by sequential strings.ReplaceAll
	// calls re-scanning already-substituted text.
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
databases:
  postgres:
    host: dbhost
    port: 5432
`)
	writeFile(t, dir, "services/myapp/config.yaml", `
databases:
  - postgres:
      database: mydb
      user: "{database}"
      env:
        dsn: DATABASE_URL
`)
	env, err := config.BuildEnv(rootPath, dir, "myapp", nil)
	require.NoError(t, err)
	assert.Equal(t, "postgres://{database}:@dbhost:5432/mydb", env["DATABASE_URL"],
		"the {user} substitution's literal value must not be re-matched by the later {database} substitution")
}

// ── databases: malformed entries fail loudly ─────────────────────────────────

func TestServiceDatabases_MalformedEntry_Errors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `databases:
  - postgres
  - redis:
      env:
        host: REDIS_HOST
`)
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)

	_, err = cfg.ServiceDatabases()
	assert.ErrorContains(t, err, "databases:",
		"a malformed entry (bare scalar instead of a type/options mapping) must not be silently dropped")
}

// ── Genuine keyring errors are surfaced, not swallowed as "not set" ──────────

func TestBuildEnv_PropagatesGenuineKeyringError_RootSecret(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
secrets:
  TS_AUTHKEY:
    required: true
`)
	env, err := config.BuildEnv(rootPath, dir, "", erroringSecretsManager())
	assert.Error(t, err, "a genuine keyring backend failure must not be silently swallowed")
	_, present := env["TS_AUTHKEY"]
	assert.False(t, present, "a secret whose lookup failed should not appear in env as empty")
}

func TestBuildEnv_PropagatesGenuineKeyringError_ServiceSecret(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", "vars:\n  DOMAIN:\n    value: example.com\n")
	writeFile(t, dir, "services/myapp/config.yaml", `
secrets:
  API_KEY:
    required: true
`)
	_, err := config.BuildEnv(rootPath, dir, "myapp", erroringSecretsManager())
	assert.Error(t, err)
}

func TestBuildEnv_PropagatesGenuineKeyringError_DBPassword(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", `
databases:
  postgres:
    host: dbhost
    port: 5432
`)
	writeFile(t, dir, "services/myapp/config.yaml", `
databases:
  - postgres:
      database: mydb
      user: myuser
      env:
        dsn: DATABASE_URL
`)
	_, err := config.BuildEnv(rootPath, dir, "myapp", erroringSecretsManager())
	assert.Error(t, err)
}
