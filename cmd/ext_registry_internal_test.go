package cmd

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtRegistry_NotBuiltUntilFirstAccess guards against the extension
// registry (and therefore every layer's repoRoot) being constructed from a
// package init() — which would run before Cobra parses --config-dir/--config
// and permanently freeze every layer to the XDG default config dir.
func TestExtRegistry_NotBuiltUntilFirstAccess(t *testing.T) {
	origConfigDir := rootFlags.configDir
	t.Cleanup(func() {
		extRegistryOnce = sync.Once{}
		extRegistryInstance = nil
		rootFlags.configDir = origConfigDir
	})

	extRegistryOnce = sync.Once{}
	extRegistryInstance = nil
	assert.Nil(t, extRegistryInstance, "registry must not be built until extRegistry() is first called")

	rootFlags.configDir = t.TempDir()
	reg1 := extRegistry()
	require.NotNil(t, reg1)
	_, ok := reg1.Get("tor")
	assert.True(t, ok, "expected tor layer to be registered")

	// A later call must reuse the same instance rather than rebuilding
	// against whatever configDir() now resolves to.
	rootFlags.configDir = t.TempDir()
	reg2 := extRegistry()
	assert.Same(t, reg1, reg2)
}
