package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/session"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start [service...]",
	Short: "Start existing stopped containers (no service: the core stack)",
	Long: `Start existing service containers without creating them first.
Use 'up' to create and start containers.

  homelab start              # core stack (start existing containers)
  homelab start jellyfin immich  # these services
  homelab start -a           # every installed service
  homelab start --group media  # all services in the "media" group

Note: This runs 'docker compose start' under the hood — containers must
already exist. Use 'homelab up' to create and start.`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		if len(args) > 0 || startFlags.all || startFlags.group != "" {
			return runServiceStart(cmd, args)
		}
		env := buildEnv(dir, "")
		composeFile := run.CoreComposeFile(dir)
		if _, err := os.Stat(composeFile); err != nil {
			return err
		}
		recordCore(dir, session.Running)
		fmt.Printf("%s Starting core stack…\n", styles.Primary.Render("→"))
		return run.Default().DockerComposeEnv(
			composeFile,
			env,
			withProfiles(dir, "start")...,
		)
	},
}

var startFlags = batchFlags{}

func runServiceStart(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, startFlags.all, startFlags.group, args)
	if err != nil {
		return err
	}
	return forEachService(root, names, func(name string) error {
		recordService(root, name, session.Running)
		fmt.Printf("%s Starting %s…\n", styles.Primary.Render("→"), styles.Bold.Render(name))
		return run.Default().DockerComposeEnv(run.ServiceComposeFile(root, name), buildEnv(root, name), "start")
	})
}

func init() {
	startCmd.Flags().BoolVarP(&startFlags.all, "all", "a", false, "Start all installed services")
	startCmd.Flags().StringVar(&startFlags.group, "group", "", "Start a named service group")
	_ = startCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
