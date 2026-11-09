package cmd

import (
	"os"

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
	Long: `Execute a command in a running service container (like 'docker exec'), on
whichever machine Swarm runs it. The container is the service's own
(<service>_<service>), or its only one.
A TTY is allocated when stdin and stdout are terminals; -T disables it.

  homelab exec jellyfin sh              # interactive shell
  homelab exec jellyfin ls -la /config  # flags after the service go to the command
  homelab exec -u root jellyfin id      # as another user
  homelab exec -T jellyfin cat x > y    # no TTY, for piping`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		if cmd.Flags().Changed("tty") {
			tty = execFlags.tty
		}
		return onTaskContainer(configDir(), name, func(id string) error {
			return dockerCLI(dockerExecArgs(id, tty, args[1:]))
		})
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
