package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
)

// a-b and a_b share DB_PASSWORD_A_B in the root vault namespace: installing
// the second would hand it the first's database password.
func TestCheckNameClash(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "services", "a-b"), 0o750))

	err := config.CheckNameClash(root, "a_b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a-b")

	assert.NoError(t, config.CheckNameClash(root, "a-b"), "the service itself is not a clash")
	assert.NoError(t, config.CheckNameClash(root, "ab"))
	assert.NoError(t, config.CheckNameClash(t.TempDir(), "a_b"), "no services dir yet")
}
