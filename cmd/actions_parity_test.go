package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/network/layers"
)

// The front ends (dashboard, GUI) offer exactly the actions in
// internal/actions. These tests keep that registry and the CLI in step: a new
// command or flag must get an action, or a reasoned entry below.

// parityExcludedCommands are runnable commands no front end offers.
var parityExcludedCommands = map[string]string{
	"":           "the root command is the dashboard / GUI itself",
	"help":       "cobra's help; front ends show Action.Help",
	"completion": "emits a shell completion script — meaningless outside a shell",
}

// parityExcludedFlags are flags no action passes.
var parityExcludedFlags = map[string]string{
	" --gui":     "opens the GUI; front ends are already running",
	"logs --tui": "the CLI's own full-screen viewer; front ends render Stream output themselves",
	"exec --tty": "auto-detected; service.shell runs in a real terminal, service.exec passes --no-tty",
}

// cmdPath is a command's path without the leading "homelab".
func cmdPath(c *cobra.Command) string {
	return strings.TrimPrefix(strings.TrimPrefix(c.CommandPath(), "homelab"), " ")
}

func walkCommands(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, s := range c.Commands() {
		walkCommands(s, fn)
	}
}

func coveredCommands() map[string]bool {
	out := map[string]bool{}
	for _, a := range actions.All() {
		for _, c := range a.Commands {
			out[c] = true
		}
	}
	return out
}

func TestActionsParity_EveryCommandAndFlagHasAnAction(t *testing.T) {
	covered := coveredCommands()
	coveredPath := map[string]bool{}
	for c := range covered {
		path, _, _ := strings.Cut(c, " --")
		coveredPath[path] = true
	}

	walkCommands(rootCmd, func(c *cobra.Command) {
		if c.Hidden || !c.Runnable() {
			return
		}
		path := cmdPath(c)
		if _, ok := parityExcludedCommands[path]; !ok {
			assert.True(t, coveredPath[path],
				"command %q has no action in internal/actions — add one, or exclude it with a reason", path)
		}
		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			key := path + " --" + f.Name
			if f.Hidden {
				return
			}
			if _, ok := parityExcludedFlags[key]; ok {
				return
			}
			if _, ok := parityExcludedCommands[path]; ok {
				return
			}
			assert.True(t, covered[key],
				"flag %q has no action in internal/actions — add it, or exclude it with a reason", key)
		})
	})
}

// TestActionsParity_CommandsExist catches the reverse: an action claiming a
// command or flag the CLI no longer has.
func TestActionsParity_CommandsExist(t *testing.T) {
	for c := range coveredCommands() {
		path, flag, hasFlag := strings.Cut(c, " --")
		found, rest, err := rootCmd.Find(strings.Fields(path))
		require.NoError(t, err, c)
		require.Empty(t, rest, "%q: no such command", c)
		require.True(t, found.Runnable(), "%q: not a runnable command", c)
		if hasFlag {
			assert.NotNil(t, found.Flags().Lookup(flag), "%q: no such flag", c)
		}
	}
}

// resetFlags puts every flag back to its default: flags are package state,
// and parsing an action's argv must not leak into later tests.
func resetFlags() {
	reset := func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	}
	walkCommands(rootCmd, func(c *cobra.Command) {
		c.Flags().VisitAll(reset)
		c.PersistentFlags().VisitAll(reset)
	})
}

// sampleInputs fills every input, so each optional flag is exercised.
func sampleInputs(a actions.Action) actions.Inputs {
	in := actions.Inputs{}
	for _, i := range a.Inputs {
		switch i.Kind {
		case actions.Bool:
			in[i.Key] = "true"
		case actions.Path:
			in[i.Key] = "/tmp/x"
		case actions.Choice:
			in[i.Key] = "media"
		default:
			in[i.Key] = "8080"
		}
	}
	return in
}

func sampleTargets(scope actions.Scope) []actions.Target {
	var all []string
	for _, l := range layers.Static() {
		all = append(all, l.Name())
	}
	switch scope {
	case actions.Service, actions.CatalogEntry:
		return []actions.Target{
			{Scope: scope, Name: "web", Running: 1, Total: 1, Exposed: all, Layers: all},
			{Scope: scope, Name: "web", Total: 1, Layers: all},
			{Scope: scope, Names: []string{"web", "db"}, Running: 1, Total: 2, Layers: all},
		}
	case actions.Layer:
		var ts []actions.Target
		for _, l := range layers.Static() {
			if l.Flag() != "" {
				ts = append(ts, actions.LayerTarget(l.Name(), true, true), actions.LayerTarget(l.Name(), true, false),
					actions.LayerTarget(l.Name(), false, false))
			}
		}
		return ts
	default:
		return []actions.Target{{Scope: scope, Layers: all}}
	}
}

// TestActions_ArgsParse checks that every action, with default and with all
// inputs filled, builds an argv the CLI accepts: a runnable command, known
// flags with valid values, and the right number of arguments. It parses
// only — nothing is executed.
func TestActions_ArgsParse(t *testing.T) {
	t.Cleanup(resetFlags)
	for _, a := range actions.All() {
		for _, tgt := range sampleTargets(a.Scope) {
			if !a.Can(tgt) {
				continue
			}
			for _, in := range []actions.Inputs{nil, sampleInputs(a)} {
				argv := a.Build(tgt, in)
				resetFlags()
				c, rest, err := rootCmd.Find(argv)
				require.NoError(t, err, "%s: %v", a.ID, argv)
				require.True(t, c.Runnable(), "%s: %v is not a runnable command", a.ID, argv)
				require.NoError(t, c.ParseFlags(rest), "%s: %v", a.ID, argv)
				require.NoError(t, c.ValidateArgs(c.Flags().Args()), "%s: %v", a.ID, argv)
				if in == nil {
					continue
				}
				// Every Bool input must reach the command as a flag.
				for _, i := range a.Inputs {
					if i.Kind == actions.Bool {
						assert.True(t, slices.Contains(argv, "--"+i.Key), "%s: input %s not passed: %v", a.ID, i.Key, argv)
					}
				}
			}
		}
	}
}
