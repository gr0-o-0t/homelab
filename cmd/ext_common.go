package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

// Shared building blocks for the per-layer command trees (cf, tor, i2p, ygg)
// and the always-on core containers.
//
// Each of those files used to hand-roll the same three things — a compose
// invocation, a `logs` subcommand, and the preamble every `status` subcommand
// opens with. Five `logs` bodies were byte-identical apart from a container
// name; four `status` preambles differed only in wording. Copies like that
// don't just cost lines, they drift: the i2p and tor status commands had
// already grown different phrasing for the same "not enabled" state.

// coreCompose runs a docker compose command against the core stack with the
// root environment and every active profile.
//
// The env matters: without it compose substitutes "" for TS_AUTHKEY and
// friends, which is harmless for `logs` and destructive for anything that
// recreates a container.
func coreCompose(args ...string) error {
	root := configDir()
	return run.Default().DockerComposeEnv(
		run.CoreComposeFile(root),
		buildEnv(root, ""),
		withProfiles(root, args...)...,
	)
}

// containerLogsCmd builds the `logs` subcommand for a core container.
func containerLogsCmd(container, short string) *cobra.Command {
	return &cobra.Command{
		Use:   "logs",
		Short: short,
		RunE: func(_ *cobra.Command, _ []string) error {
			return coreCompose("logs", "-f", container)
		},
	}
}

// requireExtEnabled reports whether an extension is enabled, printing the
// standard notice and the command that enables it when it is not.
func requireExtEnabled(root, ext, label string) bool {
	if extEnabled(root, ext) {
		return true
	}
	fmt.Printf("  %s  %s not enabled.\n", styles.Warning.Render("!"), label)
	fmt.Printf("  Run %s to enable.\n\n",
		styles.Primary.Render("homelab ext enable "+ext))
	return false
}

// requireContainerRunning prints the container's state line and reports whether
// it is running, printing the start hint when it is not.
func requireContainerRunning(container string) bool {
	state := containerStatus(container)
	if state == containerStateRunning {
		fmt.Printf("  %s  %s  %s\n",
			styles.Success.Render("✓"), container, styles.StateTag(state))
		return true
	}
	fmt.Printf("  %s  %s  %s\n",
		styles.Err.Render("✗"), container, styles.StateTag(state))
	fmt.Printf("\n  Start with: %s\n\n", styles.Primary.Render("homelab start"))
	return false
}

// extStatusHeader prints the title and the two gates every layer's `status`
// opens with. It returns false when the caller should stop — the extension is
// off, or its container isn't up — having already explained why.
func extStatusHeader(root, ext, container, title string) bool {
	fmt.Printf("\n%s\n\n", styles.Header.Render(title))
	if !requireExtEnabled(root, ext, title) {
		return false
	}
	return requireContainerRunning(container)
}
