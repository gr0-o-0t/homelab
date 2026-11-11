package swarm_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pull that stalls (registry unreachable mid-stream) must stop when ctx is
// cancelled, even if the stream's reader itself ignores ctx.
func TestDeploy_CancelledContextStopsAStalledPull(t *testing.T) {
	d, f, root := newDeployer(t)
	writeService(t, root, "box", busyboxCompose)
	f.StallPull("busybox:1")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Up(ctx, "box") }()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pulling busybox:1")
	case <-time.After(5 * time.Second):
		t.Fatal("Up stayed blocked in the image pull after its context ended")
	}
}
