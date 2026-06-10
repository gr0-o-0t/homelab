package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart [service]",
	Short: "Restart core stack or service(s)",
	Long: `Restart containers.

  homelab restart              # core stack
  homelab restart jellyfin     # one service
  homelab restart --all        # every installed service
  homelab restart --group media  # all services in the "media" group`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		// With a service arg or batch flags, restart that service.
		if len(args) > 0 || restartFlags.all || restartFlags.group != "" {
			return runServiceRestart(cmd, args)
		}
		// No arg → core stack restart.
		env := buildEnv(dir, "")
		msg := "Restarting core stack…"
		composeArgs := []string{"restart"}
		if restartFlags.build {
			msg = "Rebuilding and recreating core stack…"
			composeArgs = []string{"up", "-d", "--build"}
		}
		fmt.Printf("%s %s\n", styles.Primary.Render("→"), msg)
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, composeArgs...)...,
		)
	},
}

var restartFlags = batchFlags{}

func init() {
	restartCmd.Flags().BoolVar(&restartFlags.all, "all", false, "Restart all installed services")
	restartCmd.Flags().StringVar(&restartFlags.group, "group", "", "Restart a named service group")
	restartCmd.Flags().BoolVar(&restartFlags.build, "build", false, "Rebuild images before restarting")
	_ = restartCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
