package cmd

import (
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var pullFlags struct {
	all bool
}

var pullCmd = &cobra.Command{
	Use:   "pull [service]",
	Short: "Pull latest Docker images",
	Long: `Pull the latest Docker images for a service or core stack.
Does NOT recreate containers — use 'homelab up' after pulling.

  homelab pull              # pull core stack images
  homelab pull jellyfin     # pull one service
  homelab pull --all        # pull every installed service`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		if pullFlags.all {
			return pullAllServices(root)
		}
		if len(args) > 0 {
			name := args[0]
			if err := validateService(root, name); err != nil {
				return err
			}
			return pullOneService(root, name)
		}
		return pullCoreStack(root)
	},
}

func pullOneService(root, name string) error {
	fmt.Printf("%s Pulling images for %s\n", styles.Primary.Render("→"), styles.Bold.Render(name))
	return run.Default().DockerComposeEnv(
		run.ServiceComposeFile(root, name),
		buildEnv(root, name),
		"pull",
	)
}

func pullCoreStack(root string) error {
	fmt.Printf("%s Pulling core stack images…\n", styles.Primary.Render("→"))
	return run.Default().DockerComposeEnv(
		run.CoreComposeFile(root),
		buildEnv(root, ""),
		"pull",
	)
}

func pullAllServices(root string) error {
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
		if err := pullOneService(root, svc.Name); err != nil {
			fmt.Printf("  %s  %s: %v\n", styles.Err.Render("✗"), svc.Name, err)
			failed = append(failed, svc.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("pull failed for: %s", strings.Join(failed, ", "))
	}
	return nil
}

func init() {
	pullCmd.Flags().BoolVar(&pullFlags.all, "all", false, "Pull images for all installed services")
}
