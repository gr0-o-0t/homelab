package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/session"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop [service...]",
	Short: "Stop containers without removing them (no service: the core stack)",
	Long: `Stop running service containers without removing them.
Use 'down' to stop and remove containers.

  homelab stop              # core stack (stop containers)
  homelab stop jellyfin immich  # these services
  homelab stop -a           # every installed service
  homelab stop --group media  # all services in the "media" group

Note: This runs 'docker compose stop' under the hood — containers are
stopped but not removed. Use 'homelab down' to stop and remove.`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		if len(args) > 0 || stopFlags.all || stopFlags.group != "" {
			return runServiceStop(cmd, args)
		}
		env := buildEnv(dir, "")
		recordCore(dir, session.Stopped)
		fmt.Printf("%s Stopping core stack…\n", styles.Warning.Render("→"))
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, "stop")...,
		)
	},
}

var stopFlags = batchFlags{}

func runServiceStop(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, stopFlags.all, stopFlags.group, args)
	if err != nil {
		return err
	}
	return forEachService(root, names, func(name string) error {
		recordService(root, name, session.Stopped)
		fmt.Printf("%s Stopping %s…\n", styles.Warning.Render("→"), styles.Bold.Render(name))
		return run.Default().DockerComposeEnv(run.ServiceComposeFile(root, name), buildEnv(root, name), "stop")
	})
}

func init() {
	stopCmd.Flags().BoolVarP(&stopFlags.all, "all", "a", false, "Stop all installed services")
	stopCmd.Flags().StringVar(&stopFlags.group, "group", "", "Stop a named service group")
	_ = stopCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
