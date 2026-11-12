package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/envshim"
)

// envshimTestMain handles the re-exec modes of TestEnvshimCmd_ExecsWithMergedEnv:
// "shim" runs the command, "child" is the program it execs.
func envshimTestMain() {
	switch os.Getenv("HL_TEST_ENVSHIM") {
	case "shim":
		rootCmd.SetArgs([]string{"envshim", "--env-file", os.Getenv("HL_TEST_ENVFILE"), "--", os.Args[0]})
		_ = os.Setenv("HL_TEST_ENVSHIM", "child")
		if err := rootCmd.Execute(); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
		}
		os.Exit(3) // reached only if exec failed
	case "child":
		_, _ = os.Stdout.WriteString(os.Getenv("HL_A") + "|" + os.Getenv("HL_B") + "|" + os.Getenv("KEEP"))
		os.Exit(0)
	}
}

func TestEnvshimCmd_ExecsWithMergedEnv(t *testing.T) {
	f := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(f, envshim.Encode(map[string]string{"HL_A": "a=b", "HL_B": "l1\nl2"}), 0o600))
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "HL_TEST_ENVSHIM=shim", "HL_TEST_ENVFILE="+f, "KEEP=kept")
	out, err := c.Output()
	require.NoError(t, err)
	assert.Equal(t, "a=b|l1\nl2|kept", string(out))
}
