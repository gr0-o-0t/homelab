package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/session"
	"github.com/groot/homelab/internal/tui/styles"
)

var pruneCmd = &cobra.Command{
	Use:   "prune [service...]",
	Short: "Stop a service and reclaim its images and volumes",
	Long: `Bring a service down and reclaim what it was using: its containers, the
images they came from, and its named volumes.

  homelab prune jellyfin            # containers + images + volumes
  homelab prune jellyfin --keep-volumes
  homelab prune jellyfin immich     # several services
  homelab prune --all               # every installed service
  homelab prune --group media
  homelab prune --dangling          # only unreferenced images/build cache

DELETING VOLUMES DESTROYS DATA and cannot be undone. Volumes hold everything a
service remembers: Vaultwarden's vault, Immich's library, Forgejo's repositories.
Take a backup first:

  homelab backup <service>

Unless --keep-volumes is given you will be asked to type the service name to
confirm ("all" when several services are pruned at once). --dangling touches no service and only removes unreferenced images and
build cache, which is the safe everyday reclaim.`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runPrune,
}

var pruneFlags struct {
	all         bool
	group       string
	keepVolumes bool
	keepImages  bool
	dangling    bool
	yes         bool
}

func init() {
	pruneCmd.Flags().BoolVar(&pruneFlags.all, "all", false, "Prune all installed services")
	pruneCmd.Flags().StringVar(&pruneFlags.group, "group", "", "Prune a named service group")
	pruneCmd.Flags().BoolVar(&pruneFlags.keepVolumes, "keep-volumes", false, "Keep volumes (no data loss)")
	pruneCmd.Flags().BoolVar(&pruneFlags.keepImages, "keep-images", false, "Keep images")
	pruneCmd.Flags().BoolVar(&pruneFlags.dangling, "dangling", false,
		"Only reclaim unreferenced images and build cache; touches no service")
	pruneCmd.Flags().BoolVarP(&pruneFlags.yes, "yes", "y", false, "Skip the confirmation prompt")
	_ = pruneCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
	rootCmd.AddCommand(pruneCmd)
}

func runPrune(_ *cobra.Command, args []string) error {
	root := configDir()

	if pruneFlags.dangling {
		return pruneDangling()
	}

	names, err := resolveTargets(root, pruneFlags.all, pruneFlags.group, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("name a service, or pass --all / --group / --dangling")
	}

	// Show exactly what is about to be destroyed before asking. A prune that
	// surprises you is the one that loses the photo library.
	fmt.Printf("\n%s\n\n", styles.Header.Render("Prune"))

	volumeCount := 0
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
		plan, err := backup.PlanFor(root, name)
		if err != nil {
			return err
		}

		fmt.Printf("  %s %s\n", styles.Bold.Render(name), styles.Muted.Render("— containers"))
		if !pruneFlags.keepImages {
			fmt.Printf("      %s images\n", styles.Muted.Render("—"))
		}
		if !pruneFlags.keepVolumes {
			for _, v := range plan.Volumes {
				fmt.Printf("      %s volume %s\n", styles.Err.Render("✗"), styles.Err.Render(v))
				volumeCount++
			}
		}
	}

	// Any run that removes volumes gets the typed confirmation, not just one
	// whose plan lists some: the plan only knows volumes with an explicit
	// name:, while `compose down --volumes` also removes anonymous and
	// default-named ones. A plan saying "0 volumes" is not evidence of no data.
	if !pruneFlags.keepVolumes {
		fmt.Printf("\n  %s\n", styles.Err.Render(fmt.Sprintf(
			"%d named volume(s), plus any unnamed ones, will be DELETED. This destroys data and cannot be undone.", volumeCount)))
		fmt.Printf("  %s Back up first: %s\n",
			styles.Muted.Render("→"), styles.Primary.Render("homelab backup "+joinNames(names)))

		if !pruneFlags.yes {
			// Typing the name, rather than pressing y, is deliberate: this is
			// the one command in the CLI that can destroy a library.
			token := "all"
			if len(names) == 1 {
				token = names[0]
			}
			ok, err := confirmToken(
				fmt.Sprintf("Type %q to confirm deletion", token), token)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Println("\nAborted — nothing was removed.")
				return nil
			}
		}
	} else if !pruneFlags.yes {
		ok, err := confirm("Stop these services and remove their containers?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("\nAborted.")
			return nil
		}
	}
	fmt.Println()

	for _, name := range names {
		// `compose down` already expresses exactly this: stop + remove
		// containers, optionally the images they came from and the named volumes
		// declared in the same file. Reimplementing it with docker rm/rmi/volume
		// rm would just be a worse version that can drift from the compose file.
		downArgs := []string{"down", "--remove-orphans"}
		if !pruneFlags.keepImages {
			downArgs = append(downArgs, "--rmi", "all")
		}
		if !pruneFlags.keepVolumes {
			downArgs = append(downArgs, "--volumes")
		}

		recordService(root, name, session.Removed)
		fmt.Printf("  %s Pruning %s…\n", styles.Primary.Render("→"), styles.Bold.Render(name))
		if err := run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name), buildEnv(root, name), downArgs...,
		); err != nil {
			return fmt.Errorf("pruning %s: %w", name, err)
		}
		fmt.Printf("  %s %s pruned\n", styles.Success.Render("✓"), name)
	}

	fmt.Println()
	return nil
}

// pruneDangling reclaims space without touching any service's data.
func pruneDangling() error {
	fmt.Printf("\n%s\n\n", styles.Header.Render("Prune (unreferenced only)"))
	r := run.Default()
	if err := r.Run("docker", "image", "prune", "-f"); err != nil {
		return fmt.Errorf("pruning images: %w", err)
	}
	if err := r.Run("docker", "builder", "prune", "-f"); err != nil {
		return fmt.Errorf("pruning build cache: %w", err)
	}
	fmt.Printf("\n  %s unreferenced images and build cache removed\n\n", styles.Success.Render("✓"))
	return nil
}

// joinNames renders a target list as `homelab backup` arguments.
func joinNames(names []string) string {
	return strings.Join(names, " ")
}
