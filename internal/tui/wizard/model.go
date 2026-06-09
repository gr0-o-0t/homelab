// Package wizard implements the interactive multi-step service scaffold wizard.
package wizard

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/groot/homelab/internal/scaffold"
	"github.com/groot/homelab/internal/tui/styles"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type step int

const (
	stepName step = iota
	stepContainer
	stepPort
	stepPreview
	stepDone
)

// Model is the Bubble Tea model for the multi-step service scaffold wizard.
//
// When scaffolding completes, Scaffolded is set to true before the program exits.
type Model struct {
	repoRoot string

	step           step
	nameInput      textinput.Model
	containerInput textinput.Model
	portInput      textinput.Model

	errMsg string          // current validation / render error
	files  []scaffold.File // rendered files shown in preview step

	// Scaffolded is true when the wizard successfully wrote the service files.
	Scaffolded  bool
	ServiceName string

	width  int
	height int
}

// New returns a fresh wizard model. If initialName is non-empty it pre-fills
// the name field (used when launched from the list with 'n').
func New(repoRoot, initialName string) Model {
	ni := textinput.New()
	ni.Placeholder = "paperless"
	ni.CharLimit = 40
	ni.Width = 32
	ni.Prompt = ""
	ni.SetValue(initialName)
	ni.Focus()

	ci := textinput.New()
	ci.Placeholder = "paperless-ngx"
	ci.CharLimit = 60
	ci.Width = 32
	ci.Prompt = ""

	pi := textinput.New()
	pi.Placeholder = "8000"
	pi.CharLimit = 6
	pi.Width = 10
	pi.Prompt = ""

	return Model{
		repoRoot:       repoRoot,
		nameInput:      ni,
		containerInput: ci,
		portInput:      pi,
	}
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case stepName, stepContainer, stepPort:
			return m.updateInputStep(msg)
		case stepPreview:
			return m.updatePreviewStep(msg)
		case stepDone:
			switch msg.String() {
			case "q", "enter", "ctrl+c":
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m Model) updateInputStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit

	case "esc":
		if m.step > stepName {
			m.step--
			m.errMsg = ""
			m.focusStep(m.step)
		}
		return m, nil

	case "enter", "tab":
		if err := m.validateCurrent(); err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.errMsg = ""
		return m.advance()
	}

	// Forward keystroke to the active input.
	var cmd tea.Cmd
	switch m.step {
	case stepName:
		m.nameInput, cmd = m.nameInput.Update(msg)
	case stepContainer:
		m.containerInput, cmd = m.containerInput.Update(msg)
	case stepPort:
		m.portInput, cmd = m.portInput.Update(msg)
	}
	return m, cmd
}

func (m Model) updatePreviewStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "esc":
		m.step = stepPort
		m.portInput.Focus()
		m.errMsg = ""
		return m, nil
	case "enter":
		if err := scaffold.Write(m.repoRoot, m.files); err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.Scaffolded = true
		m.ServiceName = strings.TrimSpace(m.nameInput.Value())
		m.step = stepDone
		return m, nil
	}
	return m, nil
}

// advance moves to the next step, pre-filling container from name if needed.
func (m Model) advance() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepName:
		// Pre-fill container with the service name if the field is still empty.
		if strings.TrimSpace(m.containerInput.Value()) == "" {
			m.containerInput.SetValue(strings.TrimSpace(m.nameInput.Value()))
		}
		m.nameInput.Blur()
		m.step = stepContainer
		m.containerInput.Focus()

	case stepContainer:
		m.containerInput.Blur()
		m.step = stepPort
		m.portInput.Focus()

	case stepPort:
		data := scaffold.ServiceData{
			Name:      strings.TrimSpace(m.nameInput.Value()),
			Container: strings.TrimSpace(m.containerInput.Value()),
			Port:      strings.TrimSpace(m.portInput.Value()),
		}
		files, err := scaffold.Render(data)
		if err != nil {
			m.errMsg = fmt.Sprintf("render error: %s", err)
			return m, nil
		}
		m.portInput.Blur()
		m.files = files
		m.step = stepPreview
	}
	return m, nil
}

func (m *Model) focusStep(s step) {
	m.nameInput.Blur()
	m.containerInput.Blur()
	m.portInput.Blur()
	switch s {
	case stepName:
		m.nameInput.Focus()
	case stepContainer:
		m.containerInput.Focus()
	case stepPort:
		m.portInput.Focus()
	}
}

func (m Model) validateCurrent() error {
	switch m.step {
	case stepName:
		name := strings.TrimSpace(m.nameInput.Value())
		if name == "" {
			return fmt.Errorf("name is required")
		}
		if !nameRe.MatchString(name) {
			return fmt.Errorf("use lowercase letters, digits, and hyphens only")
		}
		if _, err := os.Stat(filepath.Join(m.repoRoot, "services", name)); err == nil {
			return fmt.Errorf("service %q already exists", name)
		}

	case stepContainer:
		if strings.TrimSpace(m.containerInput.Value()) == "" {
			return fmt.Errorf("container name is required")
		}

	case stepPort:
		port := strings.TrimSpace(m.portInput.Value())
		if port == "" {
			return fmt.Errorf("port is required")
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("port must be a number between 1 and 65535")
		}
	}
	return nil
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}
	var b strings.Builder

	b.WriteString("\n  " + styles.Header.Render("New Service") + "\n\n")
	b.WriteString("  " + m.renderStepDots() + "\n\n")

	switch m.step {
	case stepName:
		b.WriteString(renderInputBlock(
			"Service name",
			"lowercase letters, digits, and hyphens  (e.g. paperless)",
			m.nameInput.View(),
		))
	case stepContainer:
		b.WriteString(renderInputBlock(
			"Container name",
			"Docker container_name used in reverse_proxy  (e.g. paperless-ngx)",
			m.containerInput.View(),
		))
	case stepPort:
		b.WriteString(renderInputBlock(
			"Port",
			"port the container listens on  (e.g. 8000)",
			m.portInput.View(),
		))
	case stepPreview:
		b.WriteString(m.renderPreview())
	case stepDone:
		b.WriteString(m.renderDone())
	}

	if m.errMsg != "" {
		b.WriteString("\n  " + styles.Err.Render("✗ "+m.errMsg) + "\n")
	}

	if m.step < stepPreview {
		b.WriteString("\n  " + styles.Muted.Render("[enter] next  [esc] back  [ctrl+c] quit") + "\n")
	}

	return b.String()
}

func (m Model) renderStepDots() string {
	labels := []string{"name", "container", "port", "preview", "done"}
	parts := make([]string, len(labels))
	for i, label := range labels {
		cur := step(i)
		switch {
		case cur < m.step:
			parts[i] = styles.Success.Render("● " + label)
		case cur == m.step:
			parts[i] = styles.Primary.Render("● " + label)
		default:
			parts[i] = styles.Muted.Render("○ " + label)
		}
	}
	return strings.Join(parts, styles.Muted.Render("  ›  "))
}

func renderInputBlock(label, hint, inputView string) string {
	var b strings.Builder
	b.WriteString("  " + styles.Bold.Render(label) + "\n")
	b.WriteString("  " + styles.Muted.Render(hint) + "\n\n")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(styles.ColBorder).
		Padding(0, 1).
		Width(36).
		Render(inputView)
	b.WriteString("  " + box + "\n")
	return b.String()
}

func (m Model) renderPreview() string {
	var b strings.Builder
	b.WriteString("  " + styles.Bold.Render("Preview") + "  " +
		styles.Muted.Render("press [enter] to create, [esc] to go back, [q] to quit") + "\n\n")

	for _, f := range m.files {
		b.WriteString("  " + styles.Primary.Render(f.RelPath) + "\n")
		for _, line := range strings.Split(strings.TrimRight(f.Content, "\n"), "\n") {
			b.WriteString("    " + styles.Muted.Render(line) + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) renderDone() string {
	name := strings.TrimSpace(m.nameInput.Value())
	var b strings.Builder
	b.WriteString("  " + styles.Success.Render("✓") + "  Scaffolded " +
		styles.Bold.Render("services/"+name+"/") + "\n\n")
	b.WriteString("  " + styles.Muted.Render("├──") + " docker-compose.yml\n")
	b.WriteString("  " + styles.Muted.Render("├──") + " caddy.conf\n")
	b.WriteString("  " + styles.Muted.Render("└──") + " .env.example\n\n")
	b.WriteString("  " + styles.Text.Render("Next steps:") + "\n")
	fmt.Fprintf(&b, "    1. Edit %s\n",
		styles.Primary.Render(fmt.Sprintf("services/%s/docker-compose.yml", name)))
	fmt.Fprintf(&b, "    2. %s\n",
		styles.Muted.Render(fmt.Sprintf("cp services/%s/.env.example services/%s/.env", name, name)))
	fmt.Fprintf(&b, "    3. %s\n",
		styles.Primary.Render(fmt.Sprintf("homelab up %s", name)))
	fmt.Fprintf(&b, "    4. %s\n\n",
		styles.Primary.Render(fmt.Sprintf("homelab enable %s", name)))
	b.WriteString("  " + styles.Muted.Render("[enter] dismiss") + "\n")
	return b.String()
}
