package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/diagnostics"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/tui/styles"
)

// warnPortCollisions names published host ports that are already taken, before
// compose gets a chance to fail on them.
//
// It warns and returns rather than aborting the `up`. A collision stops exactly
// one container — compose starts everything else and exits non-zero at the end,
// so refusing to start the whole stack over one occupied port would trade a
// partial success for none. What was missing is the diagnosis, and that is what
// this prints: the culprit port, up front, instead of one line of
// "failed to bind host port" buried under compose's progress output.
func warnPortCollisions(files []string, env map[string]string, profiles []string) {
	dc, err := docker.New()
	if err != nil {
		return // no Docker means `up` is about to fail for a louder reason
	}
	defer func() { _ = dc.Close() }()

	for _, r := range diagnostics.RunPortChecksForFiles(files, env, profiles, dc).Results {
		if r.Status == diagnostics.StatusPass {
			continue
		}
		fmt.Printf("  %s  %s\n", styles.Warning.Render("!"), r.Message)
	}
}
