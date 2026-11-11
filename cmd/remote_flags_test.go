package cmd

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/remote"
)

// remote.ExecValueFlags must name every exec flag (its own and the root's
// persistent ones) that takes a value: the agent skips those values to find
// the service name, past which --config belongs to the container command.
// A missing one would end that prefix early and let a root --config through.
func TestExecValueFlags_MatchExecsFlagSet(t *testing.T) {
	check := func(f *pflag.Flag) {
		takesValue := f.Value.Type() != "bool"
		assert.Equal(t, takesValue, remote.ExecValueFlags["--"+f.Name], "--%s", f.Name)
		if f.Shorthand != "" {
			assert.Equal(t, takesValue, remote.ExecValueFlags["-"+f.Shorthand], "-%s", f.Shorthand)
		}
	}
	execCmd.Flags().VisitAll(check)
	execCmd.InheritedFlags().VisitAll(check)
}

// restore's "also the config files" flag is --with-config: --config is the
// root's, which the agent refuses in a forwarded argv.
func TestRestore_WithConfigFlag(t *testing.T) {
	assert.NotNil(t, restoreCmd.Flags().Lookup("with-config"))
	assert.Nil(t, restoreCmd.LocalNonPersistentFlags().Lookup("config"))
}
