package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/groot/homelab/internal/remote"
)

// On a PC the CLI does not run commands itself: it forwards its argv to the
// leader agent, which runs this binary against its state dir and streams the
// terminal back. LocalOnly commands, the root command (dashboard/GUI, whose
// buttons exec this binary and so are forwarded one by one) and cobra's own
// help/completion run here. Inside the agent (HOMELAB_AGENT=1) nothing is
// ever forwarded.

// resolveMu serialises ResolveCommand (cobra Find mutates flag sets).
var resolveMu sync.Mutex

// invokedArgs is the argv Execute was given, forwarded verbatim.
var invokedArgs []string

// forwardRun runs argv on the agent and returns its exit code; a var so
// tests can stand in for the agent.
var forwardRun = forwardToAgent

// errForwarded stops cobra once a command ran on the agent; Execute exits
// with the remote code.
type errForwarded struct{ code int }

func (e errForwarded) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func init() {
	rootCmd.PersistentPreRunE = rootPreRun
}

func rootPreRun(c *cobra.Command, _ []string) error {
	if shouldForward(c) {
		argv := invokedArgs
		if argv == nil {
			argv = os.Args[1:]
		}
		code, err := forwardRun(argv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			if code == 0 {
				code = 1
			}
		}
		return errForwarded{code: code}
	}
	migratePreRun(c)
	return nil
}

// shouldForward reports whether c runs on the agent rather than here.
func shouldForward(c *cobra.Command) bool {
	if os.Getenv("HOMELAB_AGENT") == "1" || c == rootCmd {
		return false
	}
	top := c
	for top.HasParent() && top.Parent() != rootCmd {
		top = top.Parent()
	}
	name := top.Name()
	return !remote.LocalOnly[name] && name != "help" && !strings.HasPrefix(name, "__")
}

// ResolveCommand is the agent's remote.Resolver: the canonical command path
// argv invokes through the cobra tree, aliases resolved (`rm` → delete).
// ok is false for an unknown command; an empty path is the root command.
//
// Safe for concurrent use: the agent's runner calls it from every
// connection, and cobra's Find writes pflag maps (it merges persistent flags
// lazily, and re-adds flag.CommandLine on every call) — unguarded, two
// requests at once can die of "concurrent map writes" and take the agent,
// with Caddy and Tailscale, down.
func ResolveCommand(argv []string) ([]string, bool) {
	resolveMu.Lock()
	defer resolveMu.Unlock()
	c, _, err := rootCmd.Find(argv)
	if err != nil || c == nil {
		return nil, false
	}
	var path []string
	for ; c != nil && c != rootCmd; c = c.Parent() {
		path = append([]string{c.Name()}, path...)
	}
	return path, true
}

// executeArgs runs the CLI with args, returning the process exit code.
func executeArgs(args []string) int {
	invokedArgs = args
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	var fw errForwarded
	switch {
	case err == nil:
		return 0
	case errors.As(err, &fw):
		return fw.code
	default:
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		return 1
	}
}

// forwardToAgent runs argv on the leader agent over the terminal this
// process has: raw mode and resizes when stdin and stdout are a terminal,
// plain streams otherwise.
func forwardToAgent(argv []string) (int, error) {
	c, err := remote.LoadClient(Version, remote.KeyringSecret)
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd()) //nolint:gosec // fds fit in int
	opts := remote.RunOptions{Argv: argv, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		TTY: term.IsTerminal(in) && term.IsTerminal(out)}
	if opts.TTY {
		opts.Term = os.Getenv("TERM")
		if w, h, err := term.GetSize(out); err == nil {
			opts.Rows, opts.Cols = uint16(h), uint16(w) //nolint:gosec // terminal sizes fit
		}
		if old, err := term.MakeRaw(in); err == nil {
			defer func() { _ = term.Restore(in, old) }()
		}
		resize := make(chan [2]uint16, 1)
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if w, h, err := term.GetSize(out); err == nil {
					select {
					case resize <- [2]uint16{uint16(h), uint16(w)}: //nolint:gosec // terminal sizes fit
					default:
					}
				}
			}
		}()
		opts.Resize = resize
	}
	return c.Run(ctx, opts)
}
