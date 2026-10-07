package cmd

import (
	"os"
	"testing"
)

// TestMain points every test in this binary (package cmd and cmd_test) at a
// Docker socket that does not exist.
//
// Commands under test shell out to `docker compose`, and compose names the core
// project after its directory — "core" — whatever config dir a test passes. A
// test running `homelab update` against a temp dir once recreated the real
// caddy and tailscale containers with mounts into that temp dir, taking every
// route down. No test may reach the daemon of the machine it runs on.
func TestMain(m *testing.M) {
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-test-docker.sock")
	os.Exit(m.Run())
}
