package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/run"
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

Note: for the core stack this runs 'docker compose start' — containers must
already exist. Services run on Swarm, which keeps no stopped containers:
for them start is the same as 'homelab up'.`,
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
	// Swarm keeps no stopped containers to resume: start is up.
	return forEachService(root, names, func(name string) error {
		recordService(root, name, desired.Running)
		return upOne(root, name)
	})
}

func init() {
	startCmd.Flags().BoolVarP(&startFlags.all, "all", "a", false, "Start all installed services")
	startCmd.Flags().StringVar(&startFlags.group, "group", "", "Start a named service group")
	_ = startCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
