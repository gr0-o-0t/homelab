package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/groot/homelab/internal/session"
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
	// Nor systemd: session mode's systemctl calls and unit path are fakes.
	tmp, err := os.MkdirTemp("", "homelab-test-systemd-")
	if err != nil {
		panic(err)
	}
	sessionUnitPath = func() (string, error) { return filepath.Join(tmp, "systemd", "user", session.UnitName), nil }
	sessionSystemctl = func(context.Context, ...string) (string, error) {
		return "", errors.New("systemctl is not available in tests")
	}
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
