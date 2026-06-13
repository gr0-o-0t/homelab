package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up [service]",
	Short: "Create and start containers (primary lifecycle)",
	Long: `Create and start service containers (equivalent to 'docker compose up -d').

  homelab up                    # core stack
  homelab up jellyfin           # one service
  homelab up --all              # every installed service
  homelab up --group media      # all services in the "media" group
  homelab up --build            # rebuild images before starting`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		if len(args) > 0 || upFlags.all || upFlags.group != "" {
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
		if upFlags.build {
			upArgs = append(upArgs, "--build")
		}
		return r.DockerComposeEnv(
			composeFile,
			env,
			withProfiles(dir, upArgs...)...,
		)
	},
}

var upFlags = batchFlags{}

func init() {
	upCmd.Flags().BoolVar(&upFlags.all, "all", false, "Start all installed services")
	upCmd.Flags().StringVar(&upFlags.group, "group", "", "Start a named service group")
	upCmd.Flags().BoolVar(&upFlags.build, "build", false, "Rebuild images before starting")
	_ = upCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
