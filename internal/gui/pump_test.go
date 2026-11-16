//go:build !nogui

package gui

import (
	"io"
	"strings"
	"testing"
	"time"
)

// A line past the scanner's limit must not leave the CLI blocked on a full
// pipe: the rest is drained, so the writer (the CLI) can finish.
func TestReadLines_DrainsAfterTooLong(t *testing.T) {
	pr, pw := io.Pipe()
	wrote := make(chan error, 1)
	go func() {
		_, err := io.WriteString(pw, strings.Repeat("a", 2<<20)+"\nafter\n")
		_ = pw.Close()
		wrote <- err
	}()
	var lines []string
	readLines(pr, func(l string) { lines = append(lines, l) })
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer still blocked: the pipe was not drained")
	}
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "too long") {
		t.Errorf("want a note about the dropped output, got %d lines", len(lines))
	}
}
