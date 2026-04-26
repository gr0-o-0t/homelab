// Package styles defines all Lipgloss styles used across the CLI.
// Centralising styles here keeps colours and spacing consistent between
// the plain-output and TUI code paths.
package styles

import "github.com/charmbracelet/lipgloss"

// Colour palette — Tokyo Night inspired, works on dark terminals.
var (
	ColPrimary = lipgloss.Color("#7DCFFF") // cyan
	ColSuccess = lipgloss.Color("#9ECE6A") // green
	ColWarning = lipgloss.Color("#E0AF68") // amber
	ColError   = lipgloss.Color("#F7768E") // red/pink
	ColMuted   = lipgloss.Color("#565F89") // muted purple-gray
	ColText    = lipgloss.Color("#C0CAF5") // light lavender
	ColBorder  = lipgloss.Color("#3B4261") // subtle border
	ColAccent  = lipgloss.Color("#BB9AF7") // purple accent
)

// Typography
var (
	Bold    = lipgloss.NewStyle().Bold(true)
	Muted   = lipgloss.NewStyle().Foreground(ColMuted)
	Text    = lipgloss.NewStyle().Foreground(ColText)
	Primary = lipgloss.NewStyle().Foreground(ColPrimary)
	Accent  = lipgloss.NewStyle().Foreground(ColAccent)
	Success = lipgloss.NewStyle().Foreground(ColSuccess)
	Warning = lipgloss.NewStyle().Foreground(ColWarning)
	Err     = lipgloss.NewStyle().Foreground(ColError)
)

// Table — column widths used by both the plain table and the TUI list.
const (
	ColWidthName   = 20
	ColWidthExpose = 10
	ColWidthStatus = 16
)

// Header is the app-title bar style.
var Header = lipgloss.NewStyle().
	Bold(true).
	Foreground(ColPrimary)

// TableHeader styles a column heading.
var TableHeader = lipgloss.NewStyle().
	Bold(true).
	Foreground(ColMuted)

// Divider renders a horizontal rule in the border colour.
var Divider = lipgloss.NewStyle().
	Foreground(ColBorder)

// Dot returns a coloured status indicator based on container and routing state:
//
//	running + exposed  → green  ●   (healthy, routed)
//	running + hidden   → amber  ●   (up but not yet exposed via Caddy)
//	stopped            → muted  ○   (regardless of exposure setting)
func Dot(running, enabled bool) string {
	switch {
	case running && enabled:
		return Success.Render("●")
	case running && !enabled:
		return Warning.Render("●")
	default:
		return Muted.Render("○")
	}
}

// HealthTag renders a compact health badge with appropriate colour.
func HealthTag(health string) string {
	switch health {
	case "healthy":
		return Success.Render(health)
	case "unhealthy":
		return Err.Render(health)
	case "starting":
		return Warning.Render(health)
	default:
		return Muted.Render("–")
	}
}

// StateTag renders a container state string with appropriate colour.
func StateTag(state string) string {
	switch state {
	case "running":
		return Success.Render(state)
	case "exited":
		return Muted.Render(state)
	case "restarting":
		return Warning.Render(state)
	case "created":
		return Primary.Render(state)
	default:
		return Text.Render(state)
	}
}

// Width returns a style that pads/truncates to exactly n visible characters.
func Width(n int) lipgloss.Style {
	return lipgloss.NewStyle().Width(n)
}

// PaneTitle renders a section heading inside a pane (dim underline style).
var PaneTitle = lipgloss.NewStyle().
	Bold(true).
	Foreground(ColText)

// PaneBorder is the colour used for pane separator lines.
var PaneBorder = lipgloss.NewStyle().Foreground(ColBorder)

// PaneFocusBorder is the separator colour when the pane is focused.
var PaneFocusBorder = lipgloss.NewStyle().Foreground(ColPrimary)

// Pill renders a status badge with a coloured background.
func Pill(label string, col lipgloss.Color) string {
	return lipgloss.NewStyle().
		Background(col).
		Foreground(lipgloss.Color("#1A1B26")).
		Padding(0, 1).
		Render(label)
}
