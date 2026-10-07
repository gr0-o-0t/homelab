package gui

import (
	"regexp"
	"strconv"
	"strings"
)

// Tokyo Night, as internal/tui/styles uses it (colors_test.go keeps the shared
// hues identical), plus the background shades a desktop window needs.
const (
	hexPrimary = "#7DCFFF" // cyan — styles.ColPrimary
	hexSuccess = "#9ECE6A" // green — styles.ColSuccess
	hexWarning = "#E0AF68" // amber — styles.ColWarning
	hexError   = "#F7768E" // red — styles.ColError
	hexMuted   = "#565F89" // styles.ColMuted
	hexText    = "#C0CAF5" // styles.ColText
	hexBorder  = "#3B4261" // styles.ColBorder
	hexAccent  = "#BB9AF7" // purple — styles.ColAccent

	hexBlue     = "#7AA2F7"
	hexComment  = "#737AA2" // readable secondary text
	hexBg       = "#1A1B26"
	hexBgDark   = "#16161E"
	hexBgPanel  = "#1F2335"
	hexBgRaised = "#24283B"
	hexBgHover  = "#292E42"
	hexBgActive = "#33467C" // selection
)

// rgba is a colour as four 0..1 floats.
type rgba struct{ R, G, B, A float32 }

// hexColor parses "#RRGGBB"; malformed input is opaque magenta, loud on purpose.
func hexColor(h string) rgba {
	h = strings.TrimPrefix(h, "#")
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil || len(h) != 6 {
		return rgba{1, 0, 1, 1}
	}
	return rgba{float32(v>>16&0xff) / 255, float32(v>>8&0xff) / 255, float32(v&0xff) / 255, 1}
}

func (c rgba) alpha(a float32) rgba { c.A = a; return c }

// ── command output ────────────────────────────────────────────────────────────

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)

// cleanLine strips ANSI escapes and keeps only what a terminal would show
// after carriage returns (progress bars redraw a line with \r).
func cleanLine(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.TrimRight(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ReplaceAll(s, "\t", "    ")
}

// maxOutputLines caps a job's kept output; older lines are dropped.
const maxOutputLines = 5000

// appendCapped appends line, dropping the oldest lines beyond the cap. It
// returns a new slice when it trims, so a snapshot of the old one stays valid.
func appendCapped(lines []string, line string) []string {
	if len(lines) >= maxOutputLines {
		keep := make([]string, maxOutputLines-1, maxOutputLines+256)
		copy(keep, lines[len(lines)-(maxOutputLines-1):])
		lines = keep
	}
	return append(lines, line)
}
