package cmd

import (
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var updateAllFlag bool

var serviceUpdateCmd = &cobra.Command{
	Use:   "update [service]",
	Short: "Pull latest images and restart a service",
	Long: `Pull the latest Docker images and recreate containers for a service.

  homelab service update jellyfin     # update one service
  homelab service update --all        # update every installed service`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		if updateAllFlag {
			return updateAllServices(root)
		}
		if len(args) == 0 {
			return fmt.Errorf("service name required (or use --all)")
		}
		name := args[0]
		if err := validateService(root, name); err != nil {
			return err
		}
		return updateOneService(root, name)
	},
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

func updateAllServices(root string) error {
	svcs, err := service.Discover(root)
	if err != nil {
		return err
	}
	if len(svcs) == 0 {
		fmt.Println(styles.Muted.Render("\n  No services found.\n"))
		return nil
	}

	var failed []string
	for _, svc := range svcs {
		if err := updateOneService(root, svc.Name); err != nil {
			fmt.Printf("  %s  %s: %v\n", styles.Err.Render("✗"), svc.Name, err)
			failed = append(failed, svc.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("update failed for: %s", strings.Join(failed, ", "))
	}
	return nil
}

func init() {
	serviceUpdateCmd.Flags().BoolVar(&updateAllFlag, "all", false, "Update all installed services")
	serviceCmd.AddCommand(serviceUpdateCmd)
}
