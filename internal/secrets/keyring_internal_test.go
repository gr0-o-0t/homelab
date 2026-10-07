package secrets

import (
	"testing"

	"github.com/99designs/keyring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenBackends_ReportsSelectedBackend forces the file backend (the only
// one that works headlessly in a test environment) and asserts openBackends
// reports it accurately — this is the mechanism Open() relies on to warn
// when the weaker file-backend fallback is in use.
func TestOpenBackends_ReportsSelectedBackend(t *testing.T) {
	dir := t.TempDir()

	m, err := openBackends([]keyring.BackendType{keyring.FileBackend}, dir, "test-passphrase")
	require.NoError(t, err)
	assert.Equal(t, keyring.FileBackend, m.Backend)

	require.NoError(t, m.Set("", "TEST_VAR", "secret-value"))
	val, err := m.Get("", "TEST_VAR")
	require.NoError(t, err)
	assert.Equal(t, "secret-value", val)
}

// TestOpenBackends_AllUnavailable confirms a clear error (not a panic or a
// zero-value Manager) when no backend in the list can be opened.
func TestOpenBackends_AllUnavailable(t *testing.T) {
	_, err := openBackends(nil, "", "")
	assert.Error(t, err)
}

// A SecretService → other-backend fallback used to be silent; Open must name
// the backend it settled on.
func TestFallbackWarning(t *testing.T) {
	assert.Empty(t, (&Manager{Backend: keyring.SecretServiceBackend}).fallbackWarning())

	w := (&Manager{Backend: keyring.PassBackend,
		skipped: []keyring.BackendType{keyring.SecretServiceBackend}}).fallbackWarning()
	assert.Contains(t, w, "secret-service")
	assert.Contains(t, w, "using pass backend")
	assert.NotContains(t, w, "\n")

	assert.Contains(t, (&Manager{Backend: keyring.FileBackend}).fallbackWarning(), "using file backend")
}
