package cmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
)

var pruneCmd = &cobra.Command{
	Use:   "prune [service...]",
	Short: "Stop a service and reclaim its images and volumes",
	Long: `Bring a service down and reclaim what it was using: its Swarm services, the
images they came from (on the leader) and its volumes.

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

	names, err := resolveTargets(root, "prune", pruneFlags.all, pruneFlags.group, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("Nothing to prune: no installed service matches.")
		return nil
	}

	// Show exactly what is about to be destroyed before asking. A prune that
	// surprises you is the one that loses the photo library.
	fmt.Printf("\n%s\n\n", styles.Header.Render("Prune"))

	plans := make(map[string]prunePlan, len(names))
	volumeCount := 0
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
		plan, err := planPrune(root, name)
		if err != nil {
			return err
		}
		plans[name] = plan

		fmt.Printf("  %s %s\n", styles.Bold.Render(name), styles.Muted.Render("— Swarm services, env secrets and configs"))
		if !pruneFlags.keepImages {
			for _, img := range plan.images {
				fmt.Printf("      %s image %s\n", styles.Muted.Render("—"), img)
			}
		}
		if !pruneFlags.keepVolumes {
			for _, v := range plan.volumes {
				fmt.Printf("      %s volume %s\n", styles.Err.Render("✗"), styles.Err.Render(v))
				volumeCount++
			}
		}
	}

	// Any run that removes volumes gets the typed confirmation, even one whose
	// plan lists none: a mistyped target is still worth stopping.
	if !pruneFlags.keepVolumes {
		fmt.Printf("\n  %s\n", styles.Err.Render(fmt.Sprintf(
			"%d volume(s) will be DELETED. This destroys data and cannot be undone.", volumeCount)))
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
		recordService(root, name, desired.Removed)
		fmt.Printf("  %s Pruning %s…\n", styles.Primary.Render("→"), styles.Bold.Render(name))
		if err := pruneOne(root, name, plans[name]); err != nil {
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

// prunePlan is what pruning a service removes besides its Swarm services:
// the volumes and images its compose file names.
type prunePlan struct {
	volumes []string
	images  []string
}

// planPrune reads the volumes and images from the service's compose file, as
// it deploys: a volume's name is its `name:`, or <service>_<key>.
func planPrune(root, name string) (prunePlan, error) {
	p, err := swarm.LoadProject(filepath.Join(root, "services", name), buildEnv(root, name))
	if err != nil {
		return prunePlan{}, err
	}
	var plan prunePlan
	for _, key := range slices.Sorted(maps.Keys(p.Volumes)) {
		if v := p.Volumes[key]; !bool(v.External) {
			plan.volumes = append(plan.volumes, v.Name)
		}
	}
	seen := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(p.Services)) {
		if img := p.Services[key].Image; img != "" && !seen[img] {
			seen[img] = true
			plan.images = append(plan.images, img)
		}
	}
	return plan, nil
}

// pruneOne takes a service's Swarm services down, waits for their containers
// to stop, removes them (with their env secrets and configs), then the
// volumes and images the plan names. Volumes live on the leader, where this
// runs; images are removed from the leader only — other nodes keep theirs
// until `docker image prune` there.
func pruneOne(root, name string, plan prunePlan) error {
	if err := withDeployer(root, false, func(ctx context.Context, d *swarm.Deployer) error {
		if err := d.Down(ctx, name); err != nil {
			return err
		}
		if err := d.WaitStopped(ctx, name, 2*time.Minute); err != nil {
			return err
		}
		return d.Remove(ctx, name)
	}); err != nil {
		return err
	}
	r := run.Default()
	var errs []error
	if !pruneFlags.keepVolumes {
		for _, v := range plan.volumes {
			switch err := removeVolume(r, v); {
			case errors.Is(err, errVolumeAbsent):
				fmt.Printf("    %s volume %s not found on this node\n", styles.Muted.Render("·"), v)
			case err != nil:
				errs = append(errs, fmt.Errorf("volume %s: %w", v, err))
			default:
				fmt.Printf("    %s volume %s removed\n", styles.Success.Render("✓"), v)
			}
		}
	}
	if !pruneFlags.keepImages {
		for _, img := range plan.images {
			// An image another service still uses, or one never pulled
			// here, stays: that is not a failed prune.
			if _, err := r.Output("docker", "image", "rm", img); err != nil {
				fmt.Printf("    %s image %s kept (in use or not on this node)\n", styles.Muted.Render("·"), img)
			}
		}
	}
	return errors.Join(errs...)
}

// errVolumeAbsent: the volume is not on this node (never created, or already
// gone).
var errVolumeAbsent = errors.New("volume not found on this node")

// removeVolume removes a volume, retrying while the removed tasks' containers
// are still being cleaned up and hold it. A volume this node does not have
// is errVolumeAbsent: nothing was removed.
func removeVolume(r *run.Commander, v string) error {
	if _, err := r.Output("docker", "volume", "inspect", v); err != nil {
		return errVolumeAbsent
	}
	var err error
	for range 15 {
		if _, err = r.Output("docker", "volume", "rm", v); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return err
}
