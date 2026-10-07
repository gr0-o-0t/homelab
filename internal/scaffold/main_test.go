package scaffold_test

import (
	"os"
	"testing"
)

// TestMain keeps these tests off the real Docker daemon; see cmd/main_test.go.
func TestMain(m *testing.M) {
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-test-docker.sock")
	os.Exit(m.Run())
}
