package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start [service]",
	Short: "Start existing service containers",
	Long: `Start existing service containers without creating them first.
Use 'up' to create and start containers.

  homelab start              # core stack (start existing containers)
  homelab start jellyfin     # one service
  homelab start --all        # every installed service
  homelab start --group media  # all services in the "media" group

Note: This runs 'docker compose start' under the hood — containers must
already exist. Use 'homelab up' to create and start.`,
	Args:              cobra.MaximumNArgs(1),
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
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
		fmt.Printf("%s Starting %s\n", styles.Primary.Render("→"), styles.Bold.Render(name))
		if err := run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name),
			buildEnv(root, name),
			"start",
		); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	startCmd.Flags().BoolVar(&startFlags.all, "all", false, "Start all installed services")
	startCmd.Flags().StringVar(&startFlags.group, "group", "", "Start a named service group")
	_ = startCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
