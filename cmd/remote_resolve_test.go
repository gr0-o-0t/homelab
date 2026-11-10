package cmd

import (
	"strings"
	"sync"
	"testing"

	"github.com/groot/homelab/internal/remote"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestResolveCommand_CanonicalPathThroughAliases(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		path []string
		ok   bool
	}{
		{[]string{"rm", "web", "-y"}, []string{"delete"}, true},
		{[]string{"ps"}, []string{"status"}, true},
		{[]string{"--no-color", "up", "web"}, []string{"up"}, true},
		{[]string{"cf", "route", "add", "web"}, []string{"cf", "route", "add"}, true},
		{[]string{"--json"}, nil, true},
		{[]string{"frobnicate"}, nil, false},
	} {
		path, ok := ResolveCommand(tc.argv)
		assert.Equal(t, tc.ok, ok, "%q", tc.argv)
		assert.Equal(t, tc.path, path, "%q", tc.argv)
	}
}

// The agent's lock decision through the real cobra tree: every spelling of
// backup that mentions --list (as a flag, a flag's value, a positional after
// --, or overridden later) takes the lock; only backup-list is lock-free.
func TestResolveCommand_BackupLocksWhateverTheFlags(t *testing.T) {
	for argv, want := range map[string]bool{
		"backup --list": true, "backup --list --json": true, "backup --all": true,
		"backup --all --out --list": true, "backup --group --list": true, "backup --group media --list": true,
		"backup --fetch --list": true, "backup --all -- --list": true, "backup --all --list --list=false": true,
		"backup-list --json": false, "--json backup-list": false,
	} {
		path, ok := ResolveCommand(strings.Fields(argv))
		assert.True(t, ok, argv)
		assert.Equal(t, want, remote.Locks(path), argv)
	}
}

// The agent resolves from every connection goroutine at once; cobra's Find
// writes flag maps. Run under -race. Paths are distinct per goroutine, and
// include commands no earlier test walked.
func TestResolveCommand_ConcurrentSafe(t *testing.T) {
	var paths [][]string
	walkCommands(rootCmd, func(c *cobra.Command) {
		if c != rootCmd {
			paths = append(paths, strings.Fields(cmdPath(c)))
		}
	})
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Go(func() {
			for range 3 {
				got, ok := ResolveCommand(append(append([]string{"--no-color"}, p...), "x"))
				assert.True(t, ok, "%q", p)
				assert.NotEmpty(t, got, "%q", p)
			}
		})
	}
	wg.Wait()
}

// Cobra runs only the nearest persistent pre-run hook: one set on any
// command below root would replace rootPreRun for its subtree, which would
// then run on the PC instead of being forwarded to the agent.
func TestRoot_OnlyRootHasPersistentPreRun(t *testing.T) {
	walkCommands(rootCmd, func(c *cobra.Command) {
		if c == rootCmd {
			return
		}
		assert.Nil(t, c.PersistentPreRun, "%q sets PersistentPreRun", c.CommandPath())
		assert.Nil(t, c.PersistentPreRunE, "%q sets PersistentPreRunE", c.CommandPath())
	})
}
