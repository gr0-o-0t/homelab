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
	"github.com/groot/homelab/internal/gui"
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

// runDashboardTUI launches the fullscreen dashboard. It loops so that logs,
// install and the new-service wizard run in the terminal and return to it.
func runDashboardTUI(root string) error {
	dc, _ := docker.New()
	if dc != nil {
		defer func() { _ = dc.Close() }()
	}
	catalog := catalogNames()
	layers := uiLayers(root)
	cli := selfCLI(root)

	for {
		svcs, err := discoverAll(root, dc, catalog)
		if err != nil {
			return err
		}

		model := tuiDashboard.New(root, dc, svcs, catalog, layers, func(name string) map[string]string {
			return buildEnv(root, name)
		}, cli)
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
func runLogTUI(root, serviceName string) error {
	// The viewer streams `homelab logs -f`, so it shows exactly what the CLI
	// would. An empty service name means the core stack.
	title, args := serviceName, []string{"logs", "-f", "-n", "500"}
	if serviceName == "" {
		title = "core"
	} else {
		args = append(args, serviceName)
	}
	model := tuiLogs.New(title, append(selfCLI(root), args...))
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
func runWizardTUI(root, initialName string) error {
	model := tuiWizard.New(root, initialName)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ── shared by the dashboard and the GUI ───────────────────────────────────────

// uiLayers is the private layer plus every extension enabled in config.yaml:
// the layers a front end can show and offer to enable.
func uiLayers(root string) []network.NetworkLayer {
	cfg, _ := config.Load(config.RootConfigFile(root, rootFlags.configFile))
	var layers []network.NetworkLayer
	for _, name := range extRegistry().Names() {
		layer, ok := extRegistry().Get(name)
		if ok && cfg != nil && (name == "ts" || hasResolvedExtension(cfg, name)) {
			layers = append(layers, layer)
		}
	}
	return layers
}

// selfCLI is the argv prefix front ends run actions through: this binary,
// with the same config selection. Every button is then exactly its command.
func selfCLI(root string) []string {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cli := []string{exe, "--no-color", "--config-dir", root}
	if rootFlags.configFile != "" {
		cli = append(cli, "--config", rootFlags.configFile)
	}
	return cli
}

// discoverAll lists installed services (with live container state when Docker
// is reachable) followed by catalog entries not yet installed.
func discoverAll(root string, dc *docker.Client, catalog []string) ([]service.Service, error) {
	if dc != nil {
		return service.DiscoverAllWithDocker(root, dc, catalog)
	}
	return service.DiscoverWithCatalog(root, catalog)
}

// ── scaffold ──────────────────────────────────────────────────────────────────

// scaffoldService writes boilerplate for a new service using the embedded
// templates in internal/scaffold. Used by the non-interactive CLI path.

// runGUI opens the experimental desktop GUI with the same inputs as the
// dashboard.
func runGUI(root string) error {
	dc, _ := docker.New()
	if dc != nil {
		defer func() { _ = dc.Close() }()
	}
	catalog := catalogNames()
	return gui.Run(gui.Options{
		Discover: func() ([]service.Service, error) { return discoverAll(root, dc, catalog) },
		Layers:   uiLayers(root),
		Env:      func(name string) map[string]string { return buildEnv(root, name) },
		CLI:      selfCLI(root),
	})
}
