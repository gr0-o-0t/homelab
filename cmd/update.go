package cmd

import (
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var serviceUpdateCmd = &cobra.Command{
	Use:   "update [service]",
	Short: "Pull latest images and restart core stack or a service",
	Long: `Pull the latest Docker images and recreate containers.

  homelab update                  # update core stack (tailscale, caddy, extensions)
  homelab update jellyfin         # update one service
  homelab update --all            # update every installed service
  homelab update --group media    # all services in the "media" group`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceUpdate,
}

var updateFlags = batchFlags{}

func runServiceUpdate(_ *cobra.Command, args []string) error {
	root := configDir()
	if len(args) > 0 || updateFlags.all || updateFlags.group != "" {
		names, err := resolveTargets(root, updateFlags.all, updateFlags.group, args)
		if err != nil {
			return err
		}
		var failed []string
		for _, name := range names {
			if err := updateOneService(root, name); err != nil {
				fmt.Printf("  %s  %s: %v\n", styles.Err.Render("✗"), name, err)
				failed = append(failed, name)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("update failed for: %s", strings.Join(failed, ", "))
		}
		return nil
	}
	// No args → update core stack
	return updateCoreStack(root)
}

func updateOneService(root, name string) error {
	fmt.Printf("%s Updating %s\n", styles.Primary.Render("→"), styles.Bold.Render(name))
	composeFile := run.ServiceComposeFile(root, name)
	env := buildEnv(root, name)

	fmt.Printf("  %s\n", styles.Muted.Render("pulling latest images…"))
	if err := run.Default().DockerComposeEnv(composeFile, env, "pull"); err != nil {
		return fmt.Errorf("pulling images for %s: %w", name, err)
	}

	fmt.Printf("  %s\n", styles.Muted.Render("recreating containers…"))
	if err := run.Default().DockerComposeEnv(composeFile, env, "up", "-d", "--remove-orphans"); err != nil {
		return fmt.Errorf("restarting %s: %w", name, err)
	}

	fmt.Printf("%s %s updated\n\n", styles.Success.Render("✓"), styles.Bold.Render(name))
	return nil
}

func updateCoreStack(root string) error {
	fmt.Printf("%s Updating core stack…\n", styles.Primary.Render("→"))
	env := buildEnv(root, "")
	composeFile := run.CoreComposeFile(root)

	fmt.Printf("  %s\n", styles.Muted.Render("pulling latest images…"))
	if err := run.Default().DockerComposeEnv(composeFile, env, "pull"); err != nil {
		return fmt.Errorf("pulling core images: %w", err)
	}

	fmt.Printf("  %s\n", styles.Muted.Render("recreating containers…"))
	if err := run.Default().DockerComposeEnv(composeFile, env, "up", "-d", "--remove-orphans"); err != nil {
		return fmt.Errorf("restarting core stack: %w", err)
	}

	fmt.Printf("%s Core stack updated\n", styles.Success.Render("✓"))
	return nil
}

func init() {
	serviceUpdateCmd.Flags().BoolVar(&updateFlags.all, "all", false, "Update all installed services")
	serviceUpdateCmd.Flags().StringVar(&updateFlags.group, "group", "", "Update a named service group")
	_ = serviceUpdateCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
}
