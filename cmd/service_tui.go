package cmd

import (
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/groot/homelab/internal/clistate"
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

// runDashboardTUI launches the fullscreen dashboard. Everything — logs,
// installs, the setup and new-service wizards — runs inside it: streams in its
// embedded log viewer, interactive commands by suspending it.
func runDashboardTUI(root string) error {
	// Everything shown comes from this binary's `status --json` and friends,
	// forwarded to the leader: the dashboard works from any PC.
	cli := selfCLI(root)
	model := tuiDashboard.New(tuiDashboard.Options{
		Source:  clistate.CLI{Argv: cli},
		Catalog: catalogNames(),
		Layers:  extRegistry().All(),
		CLI:     cli,
	})
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())

	// A SIGTERM or a dropped session bypasses the in-app quit keys; still
	// kill any log stream the dashboard started before exiting.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; ok {
			model.StopStreams()
			p.Quit()
		}
	}()

	final, err := p.Run()
	if fm, ok := final.(tuiDashboard.Model); ok {
		fm.StopStreams()
	}
	return err
}

// runLogTUI launches the fullscreen log viewer for a single service.
//
// The in-app "q"/ctrl+c keypress already cleans up the underlying
// `homelab logs -f` process via the model's own Update handling, but
// that path is only reached through Bubble Tea's terminal raw-mode key
// capture. A SIGINT/SIGTERM delivered outside that (a `kill <pid>`, a
// dropped SSH session) bypasses it entirely — Go's default action for those
// signals is immediate process termination with no cleanup at all. Register
// our own handler so the child process is still killed and reaped, then let
// the Program shut down cleanly (restoring the terminal) before exiting.
func runLogTUI(root, serviceName, container string) error {
	// The viewer streams `homelab logs -f`, so it shows exactly what the CLI
	// would: every container of the stack, key-prefixed, or -c's only. An
	// empty service name means the leader agent.
	title, args := serviceName, []string{"logs", "-f", "-n", "500"}
	switch {
	case serviceName == "":
		title = "agent"
	case container != "":
		title = serviceName + " / " + container
		args = append(args, "-c", container, serviceName)
	default:
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

// selfCLI is the argv prefix front ends run actions and read state through:
// this binary, which forwards each invocation to the leader agent. The agent
// refuses --config-dir/--config (it uses its own state), so they are passed
// only when commands run here (HOMELAB_AGENT=1).
func selfCLI(root string) []string {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cli := []string{exe, "--no-color"}
	if os.Getenv("HOMELAB_AGENT") == "1" {
		cli = append(cli, "--config-dir", root)
		if rootFlags.configFile != "" {
			cli = append(cli, "--config", rootFlags.configFile)
		}
	}
	return cli
}

// ── scaffold ──────────────────────────────────────────────────────────────────

// scaffoldService writes boilerplate for a new service using the embedded
// templates in internal/scaffold. Used by the non-interactive CLI path.
