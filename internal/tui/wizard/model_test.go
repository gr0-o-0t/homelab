package wizard

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/scaffold"
)

func newTestModel(initialName string) Model {
	return New("/tmp/test-homelab", initialName)
}

// ── Init ──────────────────────────────────────────────────────────────────────

func Test_Wizard_Init(t *testing.T) {
	m := newTestModel("")
	cmd := m.Init()
	assert.NotNil(t, cmd, "Init should return textinput.Blink")
}

// ── Window resize ─────────────────────────────────────────────────────────────

func Test_Wizard_WindowSize(t *testing.T) {
	m := newTestModel("")
	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m2 := updated.(Model)
	assert.Equal(t, 80, m2.width)
	assert.Equal(t, 24, m2.height)
	assert.Nil(t, cmd)
}

// ── Quit ──────────────────────────────────────────────────────────────────────

func Test_Wizard_Quit_CtrlC(t *testing.T) {
	m := newTestModel("")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	_, ok := updated.(Model)
	assert.True(t, ok)
	assert.NotNil(t, cmd, "ctrl+c should produce a tea.Quit command")
}

// ── Step navigation ───────────────────────────────────────────────────────────

func Test_Wizard_StartsAtStepName(t *testing.T) {
	m := newTestModel("")
	assert.Equal(t, stepName, m.step)
}

func Test_Wizard_EnterAdvancesStep(t *testing.T) {
	m := newTestModel("paperless")
	m.width, m.height = 80, 24

	// Step 1: name → container
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepContainer, m2.step)
}

func Test_Wizard_EscGoesBack(t *testing.T) {
	m := newTestModel("paperless")
	m.width, m.height = 80, 24
	m.step = stepPort

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m2 := updated.(Model)
	assert.Equal(t, stepContainer, m2.step, "esc from port should go back to container")
}

func Test_Wizard_EscAtFirstStepDoesNothing(t *testing.T) {
	m := newTestModel("paperless")
	m.width, m.height = 80, 24
	// Already at stepName, esc should not go to stepName-1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m2 := updated.(Model)
	assert.Equal(t, stepName, m2.step, "esc at first step should stay")
}

// ── Name validation ───────────────────────────────────────────────────────────

func Test_Wizard_NameRequired(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24

	// Enter with empty name
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepName, m2.step, "empty name should stay on name step")
	assert.Contains(t, m2.errMsg, "required")
}

func Test_Wizard_NameInvalidChars(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("My Service!")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepName, m2.step)
	assert.Contains(t, m2.errMsg, "lowercase")
}

func Test_Wizard_NameRequiresLowercase(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("UPPERCASE")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepName, m2.step, "uppercase should fail")
	assert.Contains(t, m2.errMsg, "lowercase")
}

func Test_Wizard_NameAcceptsValid(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("my-service-42")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepContainer, m2.step, "valid name should advance")
	assert.Empty(t, m2.errMsg)
}

// ── Container validation ──────────────────────────────────────────────────────

func Test_Wizard_ContainerRequired(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("paperless")
	m.step = stepContainer

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepContainer, m2.step, "empty container should stay")
	assert.Contains(t, m2.errMsg, "required")
}

func Test_Wizard_ContainerAdvances(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("paperless")
	m.step = stepContainer
	m.containerInput.SetValue("paperless-ngx")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepPort, m2.step)
	assert.Empty(t, m2.errMsg)
}

// ── Port validation ───────────────────────────────────────────────────────────

func Test_Wizard_PortRequired(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("paperless")
	m.containerInput.SetValue("paperless-ngx")
	m.step = stepPort

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepPort, m2.step, "empty port should stay")
	assert.Contains(t, m2.errMsg, "required")
}

func Test_Wizard_PortMustBeNumber(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("paperless")
	m.containerInput.SetValue("paperless-ngx")
	m.step = stepPort
	m.portInput.SetValue("abc")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepPort, m2.step)
	assert.Contains(t, m2.errMsg, "number")
}

func Test_Wizard_PortOutOfRange(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("paperless")
	m.containerInput.SetValue("paperless-ngx")
	m.step = stepPort
	m.portInput.SetValue("99999")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepPort, m2.step)
	assert.Contains(t, m2.errMsg, "65535")
}

func Test_Wizard_PortAdvances(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("paperless")
	m.containerInput.SetValue("paperless-ngx")
	m.step = stepPort
	m.portInput.SetValue("8000")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	assert.Equal(t, stepPreview, m2.step, "valid port should advance to preview")
	assert.Empty(t, m2.errMsg)
}

// ── Preview step ──────────────────────────────────────────────────────────────

func Test_Wizard_PreviewShowsFiles(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.nameInput.SetValue("paperless")
	m.containerInput.SetValue("paperless-ngx")
	m.portInput.SetValue("8000")

	// Advance through all input steps
	var updated tea.Model = m
	for _, cmd := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyEnter}, {Type: tea.KeyEnter}} {
		updated, _ = updated.Update(cmd)
	}
	m2 := updated.(Model)
	assert.Equal(t, stepPreview, m2.step)
	assert.Greater(t, len(m2.files), 0, "preview should have rendered files")

	// Check a few generated file names
	names := make([]string, len(m2.files))
	for i, f := range m2.files {
		names[i] = f.RelPath
	}
	assert.Contains(t, names, "services/paperless/docker-compose.yml")
	assert.Contains(t, names, "services/paperless/caddy.conf")
}

func Test_Wizard_PreviewEscGoesBack(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.step = stepPreview
	m.files = []scaffold.File{
		{RelPath: "services/test/docker-compose.yml", Content: "version: '3'"},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m2 := updated.(Model)
	assert.Equal(t, stepPort, m2.step, "esc from preview should go back to port")
}

func Test_Wizard_PreviewQuit(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.step = stepPreview

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	_, ok := updated.(Model)
	assert.True(t, ok)
	assert.NotNil(t, cmd, "q from preview should quit")
}

// ── Done step ─────────────────────────────────────────────────────────────────

func Test_Wizard_DoneStep_QuitOnEnter(t *testing.T) {
	m := newTestModel("")
	m.width, m.height = 80, 24
	m.step = stepDone

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, ok := updated.(Model)
	assert.True(t, ok)
	assert.NotNil(t, cmd, "enter on done step should quit")
}

// ── View ──────────────────────────────────────────────────────────────────────

func Test_Wizard_View_EmptyWhenNoWidth(t *testing.T) {
	m := newTestModel("")
	assert.Equal(t, "", m.View())
}

func Test_Wizard_View_Renders(t *testing.T) {
	m := newTestModel("")
	m.width = 80
	v := m.View()
	assert.NotEmpty(t, v)
	assert.Contains(t, v, "New Service")
	assert.Contains(t, v, "name")
}
