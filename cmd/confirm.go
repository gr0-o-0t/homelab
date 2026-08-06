package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
)

// confirmInput is the reader prompts read from. Overridable in tests.
var confirmInput = os.Stdin

// cmdContext is the context long-running commands use. Kept as a helper so the
// call sites read the same and a cancellable context can be threaded later.
func cmdContext() context.Context { return context.Background() }

// confirm asks a yes/no question, defaulting to no.
//
// Non-interactive callers (a script, a cron job) get "no" rather than a hang:
// refusing to act without an answer is the safe outcome for the destructive
// commands this guards.
func confirm(question string) (bool, error) {
	if !isTTY() {
		return false, fmt.Errorf("%s — refusing to continue without a terminal; pass --yes to confirm", question)
	}
	fmt.Printf("  %s [y/N]: ", question)

	line, err := bufio.NewReader(confirmInput).ReadString('\n')
	if err != nil {
		return false, nil // EOF / closed stdin → treat as "no"
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// confirmToken requires the operator to type an exact string. Used where a
// mistaken "y" would destroy data — typing the service name forces you to read
// what you are about to delete.
func confirmToken(question, token string) (bool, error) {
	if !isTTY() {
		return false, fmt.Errorf("%s — refusing to continue without a terminal; pass --yes to confirm", question)
	}
	fmt.Printf("  %s: ", question)

	line, err := bufio.NewReader(confirmInput).ReadString('\n')
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(line) == token, nil
}
