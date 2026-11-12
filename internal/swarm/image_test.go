package swarm_test

import (
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/swarm"
)

func TestImageIdentity(t *testing.T) {
	img := func(id, created string, layers ...string) image.InspectResponse {
		return image.InspectResponse{ID: id, Created: created, RootFS: image.RootFS{Type: "layers", Layers: layers}}
	}
	a := swarm.ImageIdentity(img("sha256:aaa", "2026-11-12T10:00:00Z", "sha256:l1", "sha256:l2"))
	b := swarm.ImageIdentity(img("sha256:bbb", "2026-11-12T10:00:00Z", "sha256:l1", "sha256:l2"))
	assert.Equal(t, a, b, "the image ID differs between image stores; the identity must not")
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, a)

	c := swarm.ImageIdentity(img("sha256:aaa", "2026-11-12T10:00:00Z", "sha256:l1", "sha256:l3"))
	assert.NotEqual(t, a, c, "a changed layer is a different image")

	assert.Empty(t, swarm.ImageIdentity(img("sha256:aaa", "2026-11-12T10:00:00Z")))
}
