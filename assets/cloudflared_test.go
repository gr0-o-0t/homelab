package assets_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/assets"
)

// The tag is whatever Dependabot last set; only the image is fixed.
func TestCloudflaredImage_FromEmbeddedCompose(t *testing.T) {
	img, err := assets.CloudflaredImage()
	require.NoError(t, err)
	assert.Regexp(t, `^cloudflare/cloudflared:\S+$`, img)
}
