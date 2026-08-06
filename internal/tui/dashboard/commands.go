package dashboard

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/routing"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/service"
)

// ── constants ─────────────────────────────────────────────────────────────────

// Bubble Tea commands: the dashboard's side effects.
//
// Everything that talks to Docker, the config directory or the routing layer
// lives here and reports back as a message, which keeps the Update loop in
// model.go a pure state machine over those messages.

func (m Model) fetchLogsCmd() tea.Cmd {
	svc := m.selectedService()
	if svc == nil || !svc.Installed {
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

func coreRefreshCmd(dc *docker.Client) tea.Cmd {
	return func() tea.Msg {
		if dc == nil {
			return coreStatusMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return coreStatusMsg{
			ts:          dc.ContainerState(ctx, "tailscale"),
			caddy:       dc.ContainerState(ctx, "caddy"),
			cloudflared: dc.ContainerState(ctx, "cloudflared"),
			tor:         dc.ContainerState(ctx, "tor"),
			i2p:         dc.ContainerState(ctx, "i2p"),
			yggdrasil:   dc.ContainerState(ctx, "yggdrasil"),
		}
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
	if svc == nil || !svc.Installed {
		return nil
	}
	return inspectCmd(m.repoRoot, m.dc, svc.Name)
}

func privateEnableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := routing.EnablePrivate(repoRoot, name, "", nil, r); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " private route enabled"}
	}
}

func privateDisableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := routing.DisablePrivate(repoRoot, name, r); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " private route disabled"}
	}
}

func publicEnableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).EnablePublic(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " public route enabled"}
	}
}

func publicDisableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).DisablePublic(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " public route disabled"}
	}
}

func bothEnableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		mgr := caddy.NewWithRunner(repoRoot, r)
		if err := mgr.Enable(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		if err := mgr.EnablePublic(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " private + public enabled"}
	}
}

func bothDisableCmd(repoRoot, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		if err := caddy.NewWithRunner(repoRoot, r).DisableBoth(name); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " all routes disabled"}
	}
}

func upCmd(repoRoot, name string, buildEnv EnvBuilderFn) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		env := resolveEnv(buildEnv, name)
		if err := r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name), env, "up", "-d",
		); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " started"}
	}
}

func downCmd(repoRoot, name string, buildEnv EnvBuilderFn) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		_ = caddy.NewWithRunner(repoRoot, r).DisableBoth(name)
		env := resolveEnv(buildEnv, name)
		if err := r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name), env, "down",
		); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " stopped"}
	}
}

func restartCmd(repoRoot, name string, buildEnv EnvBuilderFn) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		r := &run.Commander{Stdout: &buf, Stderr: &buf}
		env := resolveEnv(buildEnv, name)
		if err := r.DockerComposeEnv(
			run.ServiceComposeFile(repoRoot, name), env, "restart",
		); err != nil {
			return opErrMsg{err: err, output: buf.String()}
		}
		return opDoneMsg{msg: name + " restarted"}
	}
}
