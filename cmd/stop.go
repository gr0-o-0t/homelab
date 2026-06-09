package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:     "stop [service]",
	Aliases: []string{"down"},
	Short:   "Stop core stack or a service",
	Long: `Stop the core stack (Tailscale, Caddy, network extensions) or one or more services.

  homelab stop              # core stack
  homelab stop jellyfin     # one service
  homelab stop --all        # every installed service
  homelab stop --group media  # all services in the "media" group

Aliased as: down (homelab down jellyfin)`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		// With a service arg or batch flags, delegate to service down.
		if len(args) > 0 || stopFlags.all || stopFlags.group != "" {
			return runServiceDown(cmd, args)
		}
		env := buildEnv(dir, "")
		fmt.Printf("%s Stopping core stack…\n", styles.Warning.Render("→"))
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, "down")...,
		)
	},
}

var stopFlags struct {
	all   bool
	group string
}

func init() {
	stopCmd.Flags().BoolVar(&stopFlags.all, "all", false, "Stop all installed services")
	stopCmd.Flags().StringVar(&stopFlags.group, "group", "", "Stop a named service group")
	_ = stopCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
