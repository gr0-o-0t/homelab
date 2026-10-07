package cmd

import (
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

var execFlags struct {
	tty, noTTY    bool
	user, workdir string
	env           []string
}

var execCmd = &cobra.Command{
	Use:   "exec <service> <command> [args...]",
	Short: "Execute a command in a running service container",
	Long: `Execute a command in a running service container (equivalent to 'docker compose exec').
A TTY is allocated when stdin and stdout are terminals; -T disables it.

  homelab exec jellyfin sh              # interactive shell
  homelab exec jellyfin ls -la /config  # flags after the service go to the command
  homelab exec -u root jellyfin id      # as another user
  homelab exec -T jellyfin cat x > y    # no TTY, for piping`,
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
		// compose exec allocates a TTY by default. Turn it off when there is
		// no terminal (piping) or when asked, as docker's -T does.
		tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		if cmd.Flags().Changed("tty") {
			tty = execFlags.tty
		}
		if !tty || execFlags.noTTY {
			composeExecArgs = append(composeExecArgs, "-T")
		}
		if execFlags.user != "" {
			composeExecArgs = append(composeExecArgs, "--user", execFlags.user)
		}
		if execFlags.workdir != "" {
			composeExecArgs = append(composeExecArgs, "--workdir", execFlags.workdir)
		}
		for _, e := range execFlags.env {
			composeExecArgs = append(composeExecArgs, "--env", e)
		}
		composeExecArgs = append(composeExecArgs, name)
		composeExecArgs = append(composeExecArgs, cmdArgs...)

		return run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name),
			buildEnv(root, name),
			append([]string{"exec"}, composeExecArgs...)...,
		)
	},
}

func init() {
	// Everything after the service name belongs to the command, as with
	// docker exec: `homelab exec svc ls -la` must not parse -la as ours.
	execCmd.Flags().SetInterspersed(false)
	execCmd.Flags().BoolVarP(&execFlags.noTTY, "no-tty", "T", false, "Disable pseudo-TTY allocation")
	execCmd.Flags().StringVarP(&execFlags.user, "user", "u", "", "Run as this user")
	execCmd.Flags().StringVarP(&execFlags.workdir, "workdir", "w", "", "Working directory inside the container")
	execCmd.Flags().StringArrayVarP(&execFlags.env, "env", "e", nil, "Set environment variables")
	execCmd.Flags().BoolVarP(&execFlags.tty, "tty", "t", false, "Allocate a pseudo-TTY (auto-detected)")
}
