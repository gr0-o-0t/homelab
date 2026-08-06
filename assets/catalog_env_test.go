package assets_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
	"github.com/groot/homelab/internal/config"
)

// rootProvidedVars are injected by buildEnv from the root config, not from a
// service's own config.yaml, so a service may reference them freely.
var rootProvidedVars = map[string]bool{
	"DOMAIN": true, "HOME_SUBDOMAIN": true, "ACME_EMAIL": true,
	"TS_HOSTNAME": true, "PUID": true, "PGID": true, "TZ": true,
}

// composeVarRef matches ${NAME} and ${NAME:-default} / ${NAME?err} style
// references. Group 2 is non-empty when the reference carries a default or
// error modifier, in which case an undeclared name is harmless.
var composeVarRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?[-?+][^}]*)?\}`)

// A compose file referencing ${SOME_SECRET} that no config.yaml declares gets
// an empty string at runtime: the container starts, then fails in a way that
// looks like a bug in the upstream image rather than a typo in the catalog.
// This catches the typo at build time instead. Only references without a
// default are checked — `${FOO:-bar}` is intentionally optional.
func TestCatalogServices_ComposeVarsAreDeclared(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()

		compose, err := assets.CatalogFS.ReadFile("services/" + svc + "/docker-compose.yml")
		if err != nil {
			continue // covered by TestCatalogServices_ComposeFileUsesYmlExtension
		}
		cfgData, err := assets.CatalogFS.ReadFile("services/" + svc + "/config.yaml")
		if err != nil {
			continue // covered by TestCatalogService_HasRequiredFiles
		}

		declared := declaredNames(t, svc, cfgData)

		var missing []string
		for _, m := range composeVarRef.FindAllStringSubmatch(effectiveYAML(t, svc, compose), -1) {
			name, modifier := m[1], m[2]
			if modifier != "" || declared[name] || rootProvidedVars[name] {
				continue
			}
			missing = append(missing, name)
		}
		sort.Strings(missing)
		assert.Empty(t, missing, "service %q references undeclared vars in docker-compose.yml — "+
			"add them to config.yaml (vars/secrets/databases env) or give them a ${NAME:-default}", svc)
	}
}

// effectiveYAML round-trips a compose file through the YAML parser so the scan
// sees only config that reaches Docker: comments are dropped (a documented
// healthcheck command in a comment is not a live var reference) and anchors
// are expanded (so a var used only via an x- anchor is still seen).
func effectiveYAML(t *testing.T, svc string, compose []byte) string {
	t.Helper()

	var doc any
	require.NoError(t, yaml.Unmarshal(compose, &doc), "service %q docker-compose.yml must parse", svc)
	out, err := yaml.Marshal(doc)
	require.NoError(t, err)
	return string(out)
}

// declaredNames returns every env var name a service's config.yaml makes
// available: vars, keyring secrets, and database connection injections.
func declaredNames(t *testing.T, svc string, cfgData []byte) map[string]bool {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, cfgData, 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err, "service %q config.yaml must parse", svc)

	names := make(map[string]bool)
	for name := range cfg.Vars {
		names[name] = true
	}
	for name := range cfg.Secrets {
		names[name] = true
	}
	if cfg.Databases.Kind != 0 {
		dbs, err := cfg.ServiceDatabases()
		require.NoError(t, err, "service %q databases block must decode", svc)
		for _, entry := range dbs {
			for _, target := range entry.Env {
				names[target] = true
			}
		}
	}
	return names
}
