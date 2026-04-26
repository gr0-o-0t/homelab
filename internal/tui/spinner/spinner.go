// Package spinner provides a lightweight terminal spinner for short-lived
// blocking operations. It degrades gracefully to a plain status line when
// stdout is not a TTY (CI, pipes).
package spinner

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var frameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#7DCFFF"))

// Run shows an animated spinner with msg while fn executes in a goroutine.
// When done, the spinner line is erased — the caller is responsible for
// printing a success or error message afterward.
//
// If stdout is not a TTY the spinner animation is skipped and msg is printed
// as a plain line instead, making output clean in CI and scripts.
func Run(msg string, fn func() error) error {
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		fmt.Printf("  %s\n", msg)
		return fn()
	}

	done := make(chan error, 1)
	go func() { done <- fn() }()

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	pad := "\r" + strings.Repeat(" ", len(msg)+10) + "\r"
	i := 0
	for {
		select {
		case err := <-done:
			fmt.Print(pad)
			return err
		case <-ticker.C:
			fmt.Printf("\r  %s  %s", frameStyle.Render(frames[i%len(frames)]), msg)
			i++
		}
	}
}
