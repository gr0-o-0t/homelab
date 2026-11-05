package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/exposure"
)

// Migrating a tor-only legacy service keeps tor in its state, leaves the old
// file alone, and warns that cluster mode does not serve it.
func TestRunMigration_WarnsAboutUnservedLayers(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "services", "onion"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "services", "onion", "config.yaml"), []byte("ports:\n  - 80\n"), 0o600))
	block := filepath.Join(root, "caddy", "conf.d-tor", "onion.conf")
	require.NoError(t, os.MkdirAll(filepath.Dir(block), 0o750))
	require.NoError(t, os.WriteFile(block, []byte("http://abc.onion:8081 {\n\treverse_proxy onion:80\n}\n"), 0o600))

	var out bytes.Buffer
	n, err := runMigration(root, &out)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, out.String(), "tor")
	assert.Contains(t, out.String(), "not available in cluster mode")
	assert.FileExists(t, block)
	st, err := exposure.Load(root, "onion")
	require.NoError(t, err)
	assert.Equal(t, []string{"tor"}, st.Layers)
	assert.NoError(t, requireMigrated(root, "onion"), "kept files do not block later commands")
}
