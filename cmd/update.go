package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var serviceUpdateCmd = &cobra.Command{
	Use:   "update [service...]",
	Short: "Pull latest images and recreate the core stack or services",
	Long: `Pull the latest Docker images and recreate containers
(docker compose pull && docker compose up -d).

  homelab update                  # core stack: refresh core files, pull, rebuild, recreate
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
	// The compose file, Dockerfiles and Caddyfile in the config dir are copies
	// of the ones embedded in this binary, written by `homelab setup`. Without
	// refreshing them here, upgrading homelab would keep running the old core
	// until someone re-ran the interactive wizard. They are static assets: no
	// identities or secrets live in them.
	if _, err := os.Stat(rootConfigFile()); err != nil {
		return fmt.Errorf("no homelab config in %s — run `homelab setup` first", root)
	}
	if err := installAssets(root); err != nil {
		return fmt.Errorf("refreshing core files: %w", err)
	}
	env := buildEnv(root, "")
	composeFile := run.CoreComposeFile(root)
	// withProfiles: the extension containers are profile-gated, and without
	// their profiles compose neither pulls nor recreates them.
	if err := run.Default().DockerComposeEnv(composeFile, env, withProfiles(root, "pull")...); err != nil {
		return fmt.Errorf("pulling core images: %w", err)
	}
	// --build: Caddy, i2p and yggdrasil are built from the refreshed Dockerfiles.
	if err := run.Default().DockerComposeEnv(composeFile, env, withProfiles(root, "up", "-d", "--build", "--remove-orphans")...); err != nil {
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
