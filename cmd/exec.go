package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

var execFlags struct {
	interactive bool
	tty         bool
}

var execCmd = &cobra.Command{
	Use:   "exec <service> <command> [args...]",
	Short: "Execute a command in a running service container",
	Long: `Execute a command in a running service container (equivalent to 'docker compose exec').
TTY is auto-detected — use --interactive/--tty to override.

  homelab exec jellyfin sh              # interactive shell
  homelab exec jellyfin cat /etc/hosts  # run command, see output
  homelab exec -i jellyfin ls -la       # non-interactive, no TTY

Flags are passed through to 'docker compose exec'.`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		cmdArgs := args[1:]
		root := configDir()

		if err := validateService(root, name); err != nil {
			return err
		}

		var composeExecArgs []string

		interactive := execFlags.interactive
		tty := execFlags.tty
		if !cmd.Flags().Changed("interactive") && !cmd.Flags().Changed("tty") {
			if isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) {
				interactive = true
				tty = true
			}
		}

		if interactive {
			composeExecArgs = append(composeExecArgs, "--interactive")
		}
		if tty {
			composeExecArgs = append(composeExecArgs, "--tty")
		}

		composeExecArgs = append(composeExecArgs, name)
		composeExecArgs = append(composeExecArgs, cmdArgs...)

		fmt.Printf("%s Executing in %s…\n", styles.Primary.Render("→"), styles.Bold.Render(name))
		return run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name),
			buildEnv(root, name),
			append([]string{"exec"}, composeExecArgs...)...,
		)
	},
}

func init() {
	execCmd.Flags().BoolVarP(&execFlags.interactive, "interactive", "i", false, "Keep stdin open (auto-detected)")
	execCmd.Flags().BoolVarP(&execFlags.tty, "tty", "t", false, "Allocate a pseudo-TTY (auto-detected)")
}
