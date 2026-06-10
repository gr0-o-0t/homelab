package cmd

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/groot/homelab/internal/tui/styles"
)

// ── Health / status helpers ─────────────────────────────────────────────────

func fileExistsAt(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func dockerDaemonUp() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// containerStatus returns the Docker container state ("running", "exited", "not found", etc.).
func containerStatus(name string) string {
	out, err := exec.Command( // nosec G204 -- binary is "docker", name is service name constant
		"docker", "inspect", "--format={{.State.Status}}", name,
	).Output()
	if err != nil {
		return "not found"
	}
	return strings.TrimSpace(string(out))
}

func stateLabel(state string) string {
	switch state {
	case containerStateRunning:
		return styles.Success.Render(containerStateRunning)
	case "not found":
		return styles.Err.Render("not found")
	default:
		return styles.Warning.Render(state)
	}
}

func tailscaleIP() (string, bool) {
	out, err := exec.Command(
		"docker", "exec", "tailscale", "tailscale", "ip", "-4",
	).Output()
	if err != nil {
		return "", false
	}
	ip := strings.TrimSpace(string(out))
	return ip, ip != ""
}

// removeBrokenSymlinks scans dir for symlinks whose targets no longer exist.
// When fix is true it removes them and returns the count removed; otherwise it
// just returns the count of broken links found.
func removeBrokenSymlinks(dir string, fix bool) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		target, err := os.Readlink(path)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			count++
			if fix {
				_ = os.Remove(path)
			}
		}
	}
	return count
}
