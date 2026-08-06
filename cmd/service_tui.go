package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
	tuiDashboard "github.com/groot/homelab/internal/tui/dashboard"
	tuiLogs "github.com/groot/homelab/internal/tui/logs"
	tuiWizard "github.com/groot/homelab/internal/tui/wizard"
)

// TTY detection and the handoff into each Bubble Tea program. Kept apart from
// the command definitions so the plain-output path stays readable on its own:
// every command here has to work identically when stdout is a pipe.

func isTTY() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) && !noColor()
}

// runListTUI launches the fullscreen dashboard. It loops so that 'l' opens the
// log viewer and 'n' opens the scaffold wizard, both returning to the dashboard.

// runListTUI launches the fullscreen dashboard. It loops so that 'l' opens the
// log viewer and 'n' opens the scaffold wizard, both returning to the dashboard.
func runListTUI(root string) error {
	return runDashboardTUI(root)
}

// runDashboardTUI is the main entry point for the interactive TUI.

// runDashboardTUI is the main entry point for the interactive TUI.
func runDashboardTUI(root string) error {
	dc, _ := docker.New()
	if dc != nil {
		defer func() { _ = dc.Close() }()
	}

	catalog := catalogNames()

	// Build network layer list from registry + config for header pills.
	cfgFile := config.RootConfigFile(root, rootFlags.configFile)
	cfg, _ := config.Load(cfgFile)
	layers := make([]network.NetworkLayer, 0, len(extRegistry().Names()))
	for _, name := range extRegistry().Names() {
		if layer, ok := extRegistry().Get(name); ok {
			// Only include layers enabled in config (or always-on like ts)
			if cfg != nil && (name == "ts" || hasResolvedExtension(cfg, name)) {
				layers = append(layers, layer)
			}
		}
	}

	for {
		var svcs []service.Service
		var err error
		if dc != nil {
			svcs, err = service.DiscoverAllWithDocker(root, dc, catalog)
		} else {
			svcs, err = service.DiscoverWithCatalog(root, catalog)
		}
		if err != nil {
			return err
		}

		model := tuiDashboard.New(root, dc, svcs, catalog, layers, func(name string) map[string]string {
			return buildEnv(root, name)
		})
		p := tea.NewProgram(model, tea.WithAltScreen())
		fm, err := p.Run()
		if err != nil {
			return err
		}

		final, ok := fm.(tuiDashboard.Model)
		if !ok {
			break
		}

		switch {
		case final.SelectedForInstall != "":
			// Install the selected catalog service, then re-enter the dashboard.
			if err := runServiceAdd(nil, []string{final.SelectedForInstall}); err != nil {
				fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
			}
		case final.SelectedForLogs != "":
			if err := runLogTUI(root, final.SelectedForLogs); err != nil {
				return err
			}
		case final.SelectedForNew:
			if err := runWizardTUI(root, ""); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

// runLogTUI launches the fullscreen log viewer for a single service.
//
// The in-app "q"/ctrl+c keypress already cleans up the underlying
// `docker compose logs -f` process via the model's own Update handling, but
// that path is only reached through Bubble Tea's terminal raw-mode key
// capture. A SIGINT/SIGTERM delivered outside that (a `kill <pid>`, a
// dropped SSH session) bypasses it entirely — Go's default action for those
// signals is immediate process termination with no cleanup at all. Register
// our own handler so the child process is still killed and reaped, then let
// the Program shut down cleanly (restoring the terminal) before exiting.

// runLogTUI launches the fullscreen log viewer for a single service.
//
// The in-app "q"/ctrl+c keypress already cleans up the underlying
// `docker compose logs -f` process via the model's own Update handling, but
// that path is only reached through Bubble Tea's terminal raw-mode key
// capture. A SIGINT/SIGTERM delivered outside that (a `kill <pid>`, a
// dropped SSH session) bypasses it entirely — Go's default action for those
// signals is immediate process termination with no cleanup at all. Register
// our own handler so the child process is still killed and reaped, then let
// the Program shut down cleanly (restoring the terminal) before exiting.
func runLogTUI(root, serviceName string) error {
	model := tuiLogs.New(root, serviceName, buildEnv(root, serviceName))
	p := tea.NewProgram(model, tea.WithAltScreen())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; ok {
			model.Stop()
			p.Quit()
		}
	}()

	_, err := p.Run()
	return err
}

// runWizardTUI launches the interactive service scaffold wizard.
// initialName pre-fills the name field; pass "" to start blank.

// runWizardTUI launches the interactive service scaffold wizard.
// initialName pre-fills the name field; pass "" to start blank.
func runWizardTUI(root, initialName string) error {
	model := tuiWizard.New(root, initialName)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ── scaffold ──────────────────────────────────────────────────────────────────

// scaffoldService writes boilerplate for a new service using the embedded
// templates in internal/scaffold. Used by the non-interactive CLI path.
