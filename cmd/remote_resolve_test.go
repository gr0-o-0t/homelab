package cmd

import (
	"testing"

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
		{[]string{"session", "run"}, []string{"session", "run"}, true},
		{[]string{"--json"}, nil, true},
		{[]string{"frobnicate"}, nil, false},
	} {
		path, ok := ResolveCommand(tc.argv)
		assert.Equal(t, tc.ok, ok, "%q", tc.argv)
		assert.Equal(t, tc.path, path, "%q", tc.argv)
	}
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
