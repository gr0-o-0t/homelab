package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var serviceUpdateCmd = &cobra.Command{
	Use:   "update [service...]",
	Short: "Pull latest images and recreate the core stack or services",
	Long: `Pull the latest Docker images and recreate containers
(docker compose pull && docker compose up -d).

  homelab update                  # core stack (tailscale, caddy, enabled extensions)
  homelab update jellyfin immich  # these services
  homelab update -a               # every installed service
  homelab update --group media    # all services in the "media" group`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceUpdate,
}

var updateFlags = batchFlags{}

func runServiceUpdate(_ *cobra.Command, args []string) error {
	root := configDir()
	if len(args) == 0 && !updateFlags.all && updateFlags.group == "" {
		return updateCoreStack(root)
	}
	names, err := resolveTargets(root, updateFlags.all, updateFlags.group, args)
	if err != nil {
		return err
	}
	return forEachService(root, names, func(name string) error {
		if err := pullOneService(root, name); err != nil {
			return err
		}
		return upOne(root, name, "--remove-orphans")
	})
}

func updateCoreStack(root string) error {
	fmt.Printf("%s Updating core stack…\n", styles.Primary.Render("→"))
	env := buildEnv(root, "")
	composeFile := run.CoreComposeFile(root)
	// withProfiles: the extension containers are profile-gated, and without
	// their profiles compose neither pulls nor recreates them.
	if err := run.Default().DockerComposeEnv(composeFile, env, withProfiles(root, "pull")...); err != nil {
		return fmt.Errorf("pulling core images: %w", err)
	}
	if err := run.Default().DockerComposeEnv(composeFile, env, withProfiles(root, "up", "-d", "--remove-orphans")...); err != nil {
		return fmt.Errorf("recreating core stack: %w", err)
	}
	fmt.Printf("%s Core stack updated\n", styles.Success.Render("✓"))
	return nil
}

func init() {
	serviceUpdateCmd.Flags().BoolVarP(&updateFlags.all, "all", "a", false, "Update all installed services")
	serviceUpdateCmd.Flags().StringVar(&updateFlags.group, "group", "", "Update a named service group")
	_ = serviceUpdateCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
