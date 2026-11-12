package cmd

import (
	"context"
	"fmt"

	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var pullFlags = batchFlags{}

var pullCmd = &cobra.Command{
	Use:   "pull <service...>",
	Short: "Pull latest Docker images",
	Long: `Pull the latest Docker images for services.
Does NOT redeploy, and 'homelab up' keeps a running service on the image it
was deployed with: 'homelab update' pulls and rolls the new images.

  homelab pull jellyfin immich  # these services
  homelab pull -a               # every installed service
  homelab pull --group media    # a service group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		names, err := resolveTargets(root, "pull", pullFlags.all, pullFlags.group, args)
		if err != nil {
			return err
		}
		return forEachService(root, names, func(name string) error { return pullOneService(root, name) })
	},
}

func pullOneService(root, name string) error {
	fmt.Printf("%s Pulling images for %s\n", styles.Primary.Render("→"), styles.Bold.Render(name))
	return deploy(root, name, func(d *swarm.Deployer, ctx context.Context, svc string) error {
		skipped, err := d.Pull(ctx, svc)
		for _, ref := range skipped {
			fmt.Printf("  %s %s skipped (local image)\n", styles.Muted.Render("·"), ref)
		}
		return err
	})
}

func init() {
	pullCmd.Flags().BoolVarP(&pullFlags.all, "all", "a", false, "Pull images for all installed services")
	pullCmd.Flags().StringVar(&pullFlags.group, "group", "", "Pull a named service group")
	_ = pullCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
