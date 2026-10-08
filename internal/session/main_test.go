package session

import (
	"os"
	"testing"
)

// TestMain keeps these tests off the real Docker daemon; see cmd/main_test.go.
// Everything here runs against fakes, so this is only a guard.
func TestMain(m *testing.M) {
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-test-docker.sock")
	os.Exit(m.Run())
}
