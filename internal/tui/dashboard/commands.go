package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/service"
)

// Bubble Tea commands: the dashboard's side effects.
//
// Everything that talks to Docker, the config directory or the routing layer
// lives here and reports back as a message, which keeps the Update loop in
// model.go a pure state machine over those messages.

func (m Model) fetchLogsCmd() tea.Cmd {
	svc := m.selectedService()
	if svc == nil || !svc.Installed || isCore(svc) {
		return nil
	}
	name := svc.Name
	repoRoot := m.repoRoot
	buildEnv := m.buildEnv
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		env := resolveEnv(buildEnv, name)
		_ = r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name),
			env,
			"logs", "--tail", fmt.Sprintf("%d", logTailLines), "--no-color",
		)
		raw := strings.TrimSpace(buf.String())
		lines := strings.Split(raw, "\n")
		var kept []string
		for _, l := range lines {
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

func refreshCmd(repoRoot string, dc *docker.Client, catalogNames []string) tea.Cmd {
	return func() tea.Msg {
		var (
			svcs []service.Service
			err  error
		)
		if dc != nil {
			svcs, err = service.DiscoverAllWithDocker(repoRoot, dc, catalogNames)
		} else {
			svcs, err = service.DiscoverWithCatalog(repoRoot, catalogNames)
		}
		if err != nil {
			return opErrMsg{err: err}
		}
		return refreshedMsg{services: svcs}
	}
}

// coreRefreshCmd reports the state of each core container by name.
func coreRefreshCmd(dc *docker.Client, containers []string) tea.Cmd {
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

func coreTickCmd() tea.Cmd {
	return tea.Tick(coreRefreshSec*time.Second, func(t time.Time) tea.Msg {
		return coreTickMsg{}
	})
}

func logTickCmd() tea.Cmd {
	return tea.Tick(logRefreshSec*time.Second, func(t time.Time) tea.Msg {
		return logTickMsg{}
	})
}

func inspectCmd(repoRoot string, dc *docker.Client, name string) tea.Cmd {
	return func() tea.Msg {
		if dc == nil || name == "" {
			return containerDetailMsg{}
		}
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

func inspectTickCmd() tea.Cmd {
	return tea.Tick(inspectRefreshSec*time.Second, func(t time.Time) tea.Msg {
		return inspectTickMsg{}
	})
}

func (m Model) fetchInspectCmd() tea.Cmd {
	svc := m.selectedService()
	if svc == nil || !svc.Installed || isCore(svc) {
		return nil
	}
	return inspectCmd(m.repoRoot, m.dc, svc.Name)
}

// cliCmd runs the homelab binary itself with args, output captured.
//
// Every action the dashboard offers is a CLI command, so it runs as one. The
// dashboard used to carry its own copy of each — and the copies drifted: "stop"
// was really down-and-unroute, "start" skipped database provisioning, and
// "public" only knew the old caddy.cf.conf symlink, so it failed on every
// service that declares ports. One path means the TUI and the CLI cannot
// disagree about what an action does.
func (m Model) cliCmd(done string, args ...string) tea.Cmd {
	argv := append(append([]string{}, m.cli...), args...)
	return func() tea.Msg {
		if len(argv) == 0 {
			return opErrMsg{err: errors.New("no homelab binary configured")}
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			return opErrMsg{err: err, output: lastLine(string(out))}
		}
		return opDoneMsg{msg: done}
	}
}

// lastLine returns the last non-empty line of s — the CLI's "error: …" line.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
