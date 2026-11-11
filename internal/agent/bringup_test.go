package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/desired"
)

// N1: a service whose deploy stalls (a registry that stops answering) must
// not hold the boot bring-up, and so the server's lock, for ever.
func TestAgent_BringUpBoundsEachServicePull(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeDesired(map[string]desired.State{"alpha": desired.Running, "beta": desired.Running})
	h.a.BringUpTimeout = 150 * time.Millisecond
	h.dep.upCtx = func(ctx context.Context, svc string) error {
		if svc == "alpha" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	lock := make(chan struct{}, 1)
	lock <- struct{}{}

	start := time.Now()
	h.a.bringUp(context.Background(), h.paths, h.dep, lock)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 3*time.Second, "bounded by the timeout, not unbounded")
	assert.Contains(t, h.rec.Calls(), "up beta", "the next service still comes up")
	logs := h.rec.Logs()
	assert.Contains(t, logs, "alpha")
	assert.Contains(t, logs, "timed out after")
	assert.Contains(t, logs, "homelab up alpha")
	assert.Empty(t, lock, "bringUp releases the lock when done")
}

// N1: a mutating command waiting on the lock gets a turn between services,
// not after all of them.
func TestAgent_BringUpReleasesTheLockBetweenServices(t *testing.T) {
	h := newHarness(t, RoleLeader)
	h.writeDesired(map[string]desired.State{"alpha": desired.Running, "beta": desired.Running, "gamma": desired.Running})
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	h.dep.onUp = func(svc string) {
		if svc != "alpha" {
			return
		}
		go func() {
			lock <- struct{}{} // a mutating command
			h.rec.add("command ran")
			<-lock
		}()
		time.Sleep(100 * time.Millisecond) // let it queue on the lock
	}

	h.a.bringUp(context.Background(), h.paths, h.dep, lock)
	require.True(t, h.rec.waitFor("command ran"))

	calls := h.rec.Calls()
	idx := func(c string) int {
		for i, x := range calls {
			if x == c {
				return i
			}
		}
		return -1
	}
	assert.Less(t, idx("up alpha"), idx("command ran"))
	assert.Less(t, idx("command ran"), idx("up beta"), "the command ran between services: %v", calls)
}
