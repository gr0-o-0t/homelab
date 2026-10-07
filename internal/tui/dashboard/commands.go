package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/service"
)

// Bubble Tea commands: the dashboard's side effects.
//
// Everything that talks to Docker, the config directory or the CLI lives here
// and reports back as a message, so Update stays a pure state machine and
// never blocks.

var errNoCLI = errors.New("no homelab binary configured")

func (m Model) refreshCmd() tea.Cmd {
	root, dc, catalog, enabled := m.opt.Root, m.opt.Docker, m.opt.Catalog, m.opt.Enabled
	return func() tea.Msg {
		var (
			svcs []service.Service
			err  error
		)
		if dc != nil {
			svcs, err = service.DiscoverAllWithDocker(root, dc, catalog)
		} else {
			svcs, err = service.DiscoverWithCatalog(root, catalog)
		}
		if err != nil {
			return refreshErrMsg{err: err}
		}
		msg := refreshedMsg{services: svcs}
		if enabled != nil {
			msg.enabled = enabled()
		}
		return msg
	}
}

// coreRefreshCmd reports the state of each core container by name.
func (m Model) coreRefreshCmd() tea.Cmd {
	dc, containers := m.opt.Docker, m.coreContainers()
	return func() tea.Msg {
		states := coreStatusMsg{}
		if dc == nil {
			return states
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, c := range containers {
			states[c] = dc.ContainerState(ctx, c)
		}
		return states
	}
}

// backupsCmd lists the backups in the default directory, newest first.
func (m Model) backupsCmd() tea.Cmd {
	root := m.opt.Root
	return func() tea.Msg {
		list, err := backup.List(backup.DefaultDir(root))
		return backupsMsg{list: list, err: err}
	}
}

func coreTickCmd() tea.Cmd {
	return tea.Tick(coreRefreshSec*time.Second, func(time.Time) tea.Msg { return coreTickMsg{} })
}

func logTickCmd() tea.Cmd {
	return tea.Tick(logRefreshSec*time.Second, func(time.Time) tea.Msg { return logTickMsg{} })
}

func inspectTickCmd() tea.Cmd {
	return tea.Tick(inspectRefreshSec*time.Second, func(time.Time) tea.Msg { return inspectTickMsg{} })
}

// fetchLogsCmd tails the selected service's logs for the detail pane.
func (m Model) fetchLogsCmd() tea.Cmd {
	svc := m.selectedService()
	if m.view != viewServices || svc == nil || !svc.Installed || isCore(svc) || m.opt.Docker == nil {
		return nil
	}
	name, root, buildEnv := svc.Name, m.opt.Root, m.opt.BuildEnv
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		_ = r.DockerComposeEnv(run.ServiceComposeFile(root, name), resolveEnv(buildEnv, name),
			"logs", "--tail", fmt.Sprintf("%d", logTailLines), "--no-color")
		var kept []string
		for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if l != "" {
				kept = append(kept, l)
			}
		}
		if len(kept) > logTailLines {
			kept = kept[len(kept)-logTailLines:]
		}
		return logTailMsg{svcName: name, lines: kept}
	}
}

func (m Model) fetchInspectCmd() tea.Cmd {
	svc := m.selectedService()
	if m.view != viewServices || svc == nil || !svc.Installed || isCore(svc) || m.opt.Docker == nil {
		return nil
	}
	dc, name := m.opt.Docker, svc.Name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		summaries, err := dc.ServiceContainers(ctx, name)
		if err != nil || len(summaries) == 0 {
			return containerDetailMsg{svcName: name}
		}
		details, _ := dc.InspectContainers(ctx, summaries)
		return containerDetailMsg{svcName: name, details: details}
	}
}

// choicesCmd resolves a Choice input's dynamic Source.
func (m Model) choicesCmd(in actions.Input) tea.Cmd {
	root := m.opt.Root
	return func() tea.Msg {
		c, err := actions.Choices(root, in)
		return choicesMsg{key: in.Key, choices: c, err: err}
	}
}

// argv is the full command line for CLI args.
func (m Model) argv(args []string) []string {
	return append(slices.Clone(m.opt.CLI), args...)
}

// runCmd runs an action captured: the CLI is the only path, so the TUI and the
// CLI cannot disagree about what an action does.
func (m Model) runCmd(p pending, args []string) tea.Cmd {
	argv := m.argv(args)
	title := p.action.Label + " · " + targetTitle(p.target)
	return func() tea.Msg {
		res := result{title: title, argv: args}
		if len(m.opt.CLI) == 0 {
			res.err = errNoCLI
			return actionDoneMsg{p: p, res: res}
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() // #nosec G204 -- argv is the CLI plus registry args
		res.out, res.err = string(out), err
		return actionDoneMsg{p: p, res: res}
	}
}

// interactiveCLI is the CLI prefix for commands that own the terminal: the
// same, but with colour, since a person is looking at it.
func (m Model) interactiveCLI() []string {
	return slices.DeleteFunc(slices.Clone(m.opt.CLI), func(s string) bool { return s == "--no-color" })
}

// interactiveCmd suspends the TUI and runs args in the real terminal.
func (m Model) interactiveCmd(label string, args []string) tea.Cmd {
	cli := m.interactiveCLI()
	if len(cli) == 0 {
		return func() tea.Msg { return execDoneMsg{label: label, err: errNoCLI} }
	}
	argv := append(cli, args...)
	c := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is the CLI plus registry args
	return tea.ExecProcess(c, func(err error) tea.Msg { return execDoneMsg{label: label, err: err} })
}

// setupLoadCmd reads `homelab setup [svc] --json`.
func (m Model) setupLoadCmd(svc string) tea.Cmd {
	args := []string{"setup"}
	if svc != "" {
		args = append(args, svc)
	}
	args = append(args, "--json")
	argv := m.argv(args)
	return func() tea.Msg {
		if len(m.opt.CLI) == 0 {
			return setupLoadedMsg{svc: svc, err: errNoCLI}
		}
		var stderr bytes.Buffer
		c := exec.Command(argv[0], argv[1:]...) // #nosec G204
		c.Stderr = &stderr
		out, err := c.Output()
		if err != nil {
			return setupLoadedMsg{svc: svc, err: fmt.Errorf("%w: %s", err, lastLine(stderr.String()))}
		}
		var info setupInfo
		if err := json.Unmarshal(out, &info); err != nil {
			return setupLoadedMsg{svc: svc, err: err}
		}
		return setupLoadedMsg{svc: svc, info: info}
	}
}

type setupLoadedMsg struct {
	svc  string
	info setupInfo
	err  error
}

// setupSaveArgs is the argv that saves a setup form. Secret values are never
// on it: they go as JSON on stdin (--secrets-stdin).
func setupSaveArgs(svc string, sets []string, secrets map[string]string) []string {
	args := []string{"setup"}
	if svc != "" {
		args = append(args, svc)
	}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	if len(secrets) > 0 {
		args = append(args, "--secrets-stdin")
	}
	return args
}

// setupSaveCmd saves vars with --set and secrets over stdin.
func (m Model) setupSaveCmd(p pending, svc string, sets []string, secrets map[string]string) tea.Cmd {
	args := setupSaveArgs(svc, sets, secrets)
	argv := m.argv(args)
	title := "Save settings · " + targetTitle(p.target)
	return func() tea.Msg {
		res := result{title: title, argv: args}
		if len(m.opt.CLI) == 0 {
			res.err = errNoCLI
			return actionDoneMsg{p: p, res: res}
		}
		c := exec.Command(argv[0], argv[1:]...) // #nosec G204
		if len(secrets) > 0 {
			data, err := json.Marshal(secrets)
			if err != nil {
				res.err = err
				return actionDoneMsg{p: p, res: res}
			}
			c.Stdin = bytes.NewReader(data)
		}
		out, err := c.CombinedOutput()
		res.out, res.err = string(out), err
		return actionDoneMsg{p: p, res: res}
	}
}

// lastLine returns the last non-empty line of s — the CLI's "error: …" line.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
