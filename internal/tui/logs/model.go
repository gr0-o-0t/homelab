// Package logs implements the fullscreen log viewer TUI.
package logs

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
)

const maxLines = 2000 // keep the last N lines to bound memory

// ── messages ──────────────────────────────────────────────────────────────────

type logLineMsg struct{ line string }
type logEndMsg struct{}

// ── model ─────────────────────────────────────────────────────────────────────

// Model is the Bubble Tea model for the log viewer.
// Call New() to create one; the log stream goroutine starts immediately.
type Model struct {
	viewport    viewport.Model
	serviceName string
	lines       []string
	logCh       <-chan string
	stopFn      func()
	ready       bool
	following   bool // auto-scroll to newest line
	width       int
	height      int
}

// New creates a Model and starts the log stream goroutine.
// env is the full environment map for docker compose; pass nil to fall back
// to the legacy --env-file behaviour using the root .env file.
func New(repoRoot, serviceName string, env map[string]string) Model {
	ch, stop := startLogStream(repoRoot, serviceName, env)
	return Model{
		serviceName: serviceName,
		logCh:       ch,
		stopFn:      stop,
		following:   true,
	}
}

// Stop terminates the underlying log-stream process. Safe to call more than
// once (startLogStream's stop closure is itself idempotent) and safe to call
// concurrently with a running Program — exported so callers can hook OS
// signal handling (SIGINT/SIGTERM delivered outside the terminal's raw-mode
// key capture, e.g. `kill <pid>` or a dropped SSH session) to guarantee the
// child process is killed even when Bubble Tea's own shutdown doesn't route
// through this model's Update (only the in-app "q"/ctrl+c keypress does).
func (m Model) Stop() {
	if m.stopFn != nil {
		m.stopFn()
	}
}

func (m Model) Init() tea.Cmd {
	return waitForLine(m.logCh)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		vw, vh := msg.Width-2, viewportHeight(msg.Height)
		if !m.ready {
			m.viewport = viewport.New(vw, vh)
			m.viewport.Style = lipgloss.NewStyle().PaddingLeft(1)
			m.ready = true
		} else {
			m.viewport.Width = vw
			m.viewport.Height = vh
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.stopFn()
			return m, tea.Quit

		case "G", "end":
			m.following = true
			m.viewport.GotoBottom()

		case "g", "home":
			m.following = false
			m.viewport.GotoTop()

		default:
			// Any manual scroll disables auto-follow. Check AtBottom() after
			// applying the keystroke, not before — otherwise scrolling away
			// from the bottom doesn't disable follow until the *next*
			// keystroke, since the pre-scroll position was still "at bottom".
			var vpCmd tea.Cmd
			m.viewport, vpCmd = m.viewport.Update(msg)
			cmds = append(cmds, vpCmd)
			if !m.viewport.AtBottom() {
				m.following = false
			}
		}

	case logLineMsg:
		m.lines = append(m.lines, msg.line)
		if len(m.lines) > maxLines {
			m.lines = m.lines[len(m.lines)-maxLines:]
		}
		if m.ready {
			m.viewport.SetContent(strings.Join(m.lines, "\n"))
			if m.following {
				m.viewport.GotoBottom()
			}
		}
		// Chain: wait for the next line.
		cmds = append(cmds, waitForLine(m.logCh))

	case logEndMsg:
		// Stream ended (process exited or was killed) — nothing more to do.

	default:
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		cmds = append(cmds, vpCmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	if !m.ready {
		return "\n  " + styles.Primary.Render("Connecting to log stream…") + "\n"
	}

	var b strings.Builder

	// ── header ────────────────────────────────────────────────────────────────
	followStr := styles.Muted.Render("scroll")
	if m.following {
		followStr = styles.Success.Render("following")
	}
	header := fmt.Sprintf("\n  %s  %s  %s  %s\n\n",
		styles.Header.Render("Logs:"),
		styles.Bold.Render(m.serviceName),
		styles.Muted.Render(fmt.Sprintf("%d lines", len(m.lines))),
		followStr,
	)
	b.WriteString(header)

	// ── viewport ──────────────────────────────────────────────────────────────
	b.WriteString(m.viewport.View())

	// ── footer ────────────────────────────────────────────────────────────────
	b.WriteString("\n\n  ")
	b.WriteString(
		styles.Muted.Render("[") + styles.Primary.Render("j/k") + styles.Muted.Render("] scroll  ") +
			styles.Muted.Render("[") + styles.Primary.Render("G") + styles.Muted.Render("] follow  ") +
			styles.Muted.Render("[") + styles.Primary.Render("g") + styles.Muted.Render("] top  ") +
			styles.Muted.Render("[") + styles.Primary.Render("q") + styles.Muted.Render("] back"),
	)
	b.WriteString("\n")

	return b.String()
}

// ── helpers ───────────────────────────────────────────────────────────────────

func viewportHeight(total int) int {
	h := total - 7 // header (3) + footer (2) + padding (2)
	if h < 1 {
		return 1
	}
	return h
}

// startLogStream launches `docker compose logs -f` in a goroutine and returns
// a channel of log lines plus a stop function. The goroutine exits when the
// stop function is called or the process exits naturally.
// env is the full environment map; if non-nil a temp file is written and
// passed via --env-file. When nil, the root .env file is used as fallback.
func startLogStream(repoRoot, serviceName string, env map[string]string) (<-chan string, func()) {
	ch := make(chan string, 256)
	done := make(chan struct{})

	go func() {
		defer close(ch)

		args := []string{"compose", "-f", run.ServiceComposeFile(repoRoot, serviceName)}

		if len(env) > 0 {
			// Write temp env file for the lifetime of this goroutine.
			if tmp, err := os.CreateTemp("", "homelab-env-*.env"); err == nil {
				tmpName := tmp.Name()
				defer func() { _ = os.Remove(tmpName) }()
				for k, v := range env {
					_, _ = fmt.Fprintf(tmp, "%s=%s\n", k, v)
				}
				_ = tmp.Close()
				_ = os.Chmod(tmpName, 0o600)
				args = append(args, "--env-file", tmpName)
			}
		} else if envFile := filepath.Join(repoRoot, ".env"); func() bool { _, e := os.Stat(envFile); return e == nil }() {
			args = append(args, "--env-file", envFile)
		}
		args = append(args, "logs", "-f")

		cmd := exec.Command("docker", args...) // nosec G204 -- binary is "docker", args are programmatic

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		cmd.Stderr = cmd.Stdout // merge stderr into stdout pipe

		if err := cmd.Start(); err != nil {
			return
		}

		// cmd.Wait() must be called exactly once. The scan loop below calls
		// it on the normal exit path (stdout EOF); the kill goroutine calls
		// it after forcing termination. sync.Once makes whichever happens
		// first the one that actually reaps the process, instead of the
		// done-channel exit path (select's <-done case, below) skipping
		// Wait() entirely and leaving a zombie.
		var waitOnce sync.Once
		reap := func() { waitOnce.Do(func() { _ = cmd.Wait() }) }

		// Kill process when stop is called.
		go func() {
			<-done
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			reap()
		}()

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			select {
			case ch <- line:
			case <-done:
				return
			}
		}
		reap()
	}()

	return ch, func() {
		select {
		case <-done: // already stopped
		default:
			close(done)
		}
	}
}

// waitForLine returns a tea.Cmd that blocks until a line arrives on ch.
// It chains itself so the Update loop keeps receiving lines.
func waitForLine(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return logEndMsg{}
		}
		return logLineMsg{line: line}
	}
}
