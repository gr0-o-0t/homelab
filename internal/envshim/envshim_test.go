package envshim_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/envshim"
)

func TestEnvShim_RoundTripsAwkwardValues(t *testing.T) {
	in := map[string]string{
		"EQ": "a=b", "NL": "line1\nline2", "Q": `"q"`, "LEAD": "  lead", "EMPTY": "", "NUL": "x\x00y",
	}
	out, err := envshim.Decode(envshim.Encode(in))
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestEnvShim_EncodeIsDeterministic(t *testing.T) {
	a := envshim.Encode(map[string]string{"A": "1", "B": "2", "C": "3"})
	b := envshim.Encode(map[string]string{"C": "3", "B": "2", "A": "1"})
	assert.Equal(t, a, b)
}

func TestEnvShim_DecodeRejectsTruncated(t *testing.T) {
	enc := envshim.Encode(map[string]string{"KEY": "value"})
	_, err := envshim.Decode(enc[:len(enc)-2])
	assert.Error(t, err)
}

func TestSecretName(t *testing.T) {
	n := envshim.SecretName("gitea", []byte("x"))
	assert.Regexp(t, `^hl_gitea_env_[0-9a-f]{12}$`, n)
	assert.NotEqual(t, n, envshim.SecretName("gitea", []byte("y")))
}
