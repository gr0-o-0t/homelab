package cmd

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withStdin swaps confirmInput for a pipe carrying body.
func withStdin(t *testing.T, body string) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString(body)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	prev := confirmInput
	confirmInput = r
	t.Cleanup(func() {
		confirmInput = prev
		_ = r.Close()
	})
}

// `homelab prune` deletes volumes, so a confirmation that defaults to yes — or
// that silently proceeds when there is no terminal — would be a data-loss bug.
// Tests run without a TTY, which is exactly the non-interactive case to pin.
func TestConfirm_NonInteractiveRefuses(t *testing.T) {
	withStdin(t, "y\n")

	ok, err := confirm("Delete everything?")
	assert.False(t, ok)
	require.Error(t, err, "must not proceed without a terminal")
	assert.ErrorContains(t, err, "--yes", "the error should name the opt-in flag")
}

func TestConfirmToken_NonInteractiveRefuses(t *testing.T) {
	withStdin(t, "jellyfin\n")

	ok, err := confirmToken("Type \"jellyfin\" to confirm", "jellyfin")
	assert.False(t, ok)
	require.Error(t, err)
	assert.ErrorContains(t, err, "--yes")
}
