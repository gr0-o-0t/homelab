package cmd

import (
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/tui/styles"
)

// ── Health / status helpers ─────────────────────────────────────────────────

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
