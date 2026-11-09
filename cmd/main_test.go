package cmd

import (
	"os"
	"testing"
)

// TestMain points every test in this binary (package cmd and cmd_test) at a
// Docker socket that does not exist: commands under test talk to Docker (the
// Swarm API, docker CLI shell-outs), and no test may reach the daemon of the
// machine it runs on.
func TestMain(m *testing.M) {
	envshimTestMain()
	_ = os.Setenv("DOCKER_HOST", "unix:///nonexistent/homelab-test-docker.sock")
	// Commands under test run in-process, as inside the agent: without this
	// every one would be forwarded to a leader (remote_test.go clears it).
	_ = os.Setenv("HOMELAB_AGENT", "1")
	os.Exit(m.Run())
}
