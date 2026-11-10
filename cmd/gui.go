package cmd

import (
	"github.com/groot/homelab/internal/clistate"
	"github.com/groot/homelab/internal/gui"
)

// runGUI opens the desktop GUI. Like the dashboard it reads everything
// through this binary (forwarded to the leader), so it works from any PC.
func runGUI(root string) error {
	cli := selfCLI(root)
	return gui.Run(gui.Options{
		Source:    clistate.CLI{Argv: cli},
		Catalog:   catalogNames(),
		AllLayers: extRegistry().All(),
		CLI:       cli,
	})
}
