package gui

import (
	"path/filepath"
	"strings"
)

// Interactive actions (a shell, the setup and new-service wizards) need a
// real terminal. The GUI opens one running the CLI argv, held open after the
// command exits so its last output can be read.

// terminalCandidates are tried in order after $TERMINAL.
var terminalCandidates = []string{"x-terminal-emulator", "kitty", "gnome-terminal", "konsole", "alacritty", "wezterm", "foot", "xterm"}

// holdScript keeps the window open after the command exits.
const holdScript = `"$@"; s=$?; printf '\n[exited with status %d — press Enter to close] ' "$s"; read -r _`

// terminalArgv returns the argv that opens a terminal emulator running cmd,
// or ok=false when none is found. getenv and lookPath are os.Getenv and
// exec.LookPath, injectable for tests.
func terminalArgv(cmd []string, getenv func(string) string, lookPath func(string) (string, error)) (argv []string, ok bool) {
	held := append([]string{"sh", "-c", holdScript, "homelab"}, cmd...)
	try := terminalCandidates
	if t := strings.TrimSpace(getenv("TERMINAL")); t != "" {
		try = append([]string{t}, try...)
	}
	for _, name := range try {
		path, err := lookPath(name)
		if err != nil {
			continue
		}
		return append([]string{path}, append(execFlag(filepath.Base(name)), held...)...), true
	}
	return nil, false
}

// execFlag is how a terminal emulator is told what to run.
func execFlag(term string) []string {
	switch term {
	case "gnome-terminal", "kgx", "ptyxis":
		return []string{"--"}
	case "kitty", "foot":
		return nil // the program follows the options directly
	case "wezterm":
		return []string{"start", "--"}
	}
	return []string{"-e"} // xterm, konsole, alacritty, x-terminal-emulator, …
}

// shellQuote renders argv as a command line to copy into a shell.
func shellQuote(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.IndexFunc(a, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r))
		}) < 0 {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}
