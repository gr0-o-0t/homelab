package routing_test

import (
	"os"
	"testing"
)

// TestMain keeps these tests off the real Docker daemon: the tor layer falls
// back to `docker exec` for an onion it cannot read; see cmd/main_test.go.
func TestMain(m *testing.M) {
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-test-docker.sock")
	os.Exit(m.Run())
}
