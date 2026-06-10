package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:     "start [service]",
	Aliases: []string{"up"},
	Short:   "Start core stack or a service",
	Long: `Start the core stack (Tailscale, Caddy, network extensions) or one or more services.

  homelab start              # core stack
  homelab start jellyfin     # one service
  homelab start --all        # every installed service
  homelab start --group media  # all services in the "media" group

Aliased as: up (homelab up jellyfin)`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		// With a service arg or batch flags, delegate to service up.
		if len(args) > 0 || startFlags.all || startFlags.group != "" {
			return runServiceUp(cmd, args)
		}
		env := buildEnv(dir, "")
		r := run.Default()

		composeFile := run.CoreComposeFile(dir)
		if _, err := os.Stat(composeFile); err != nil {
			return err
		}

		fmt.Printf("%s Starting core stack…\n", styles.Primary.Render("→"))
		for _, note := range activeExtNotes(dir) {
			fmt.Printf("  %s\n", styles.Muted.Render(note))
		}

		upArgs := []string{"up", "-d"}
		if startFlags.build {
			upArgs = append(upArgs, "--build")
		}
		return r.DockerComposeEnv(
			composeFile,
			env,
			withProfiles(dir, upArgs...)...,
		)
	},
}

var startFlags = batchFlags{}

func init() {
	startCmd.Flags().BoolVar(&startFlags.all, "all", false, "Start all installed services")
	startCmd.Flags().StringVar(&startFlags.group, "group", "", "Start a named service group")
	startCmd.Flags().BoolVar(&startFlags.build, "build", false, "Rebuild images before starting")
	_ = startCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
