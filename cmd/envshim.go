package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/envshim"
)

var envshimFile string

// envshimCmd runs inside Swarm containers (from the /hl volume): it loads the
// environment from the secret, then replaces itself with the real program.
var envshimCmd = &cobra.Command{
	Use:    "envshim -- <argv>...",
	Short:  "Set the environment from a Swarm secret and exec a program (container plumbing)",
	Hidden: true,
	Args:   cobra.MinimumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		raw, err := os.ReadFile(envshimFile)
		if err != nil {
			return err
		}
		vars, err := envshim.Decode(raw)
		if err != nil {
			return err
		}
		for k, v := range vars {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
		path, err := exec.LookPath(args[0])
		if err != nil {
			return err
		}
		if err := syscall.Exec(path, args, os.Environ()); err != nil {
			return fmt.Errorf("exec %s: %w", path, err)
		}
		return nil
	},
}

func init() {
	envshimCmd.Flags().StringVar(&envshimFile, "env-file", "/run/secrets/homelab-env", "encoded environment file")
	_ = envshimCmd.Flags().MarkHidden("env-file")
	rootCmd.AddCommand(envshimCmd)
}
