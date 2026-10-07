package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/routing"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:     "delete <service>...",
	Aliases: []string{"rm"},
	Short:   "Remove a service entirely: containers, exposure and config",
	Long: `Remove a service from homelab: take its containers down, remove every
network layer, and delete its directory from the config dir.

Unlike docker rm this deletes configuration, so it asks you to type the
service name first. Volumes are kept; use 'homelab prune' to remove them.

  homelab delete jellyfin
  homelab rm -y jellyfin          # no prompt
  homelab rm --force jellyfin     # delete even if 'down' fails`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runDelete,
}

var deleteFlags struct{ yes, force bool }

func init() {
	deleteCmd.Flags().BoolVarP(&deleteFlags.yes, "yes", "y", false, "Skip the confirmation prompt")
	deleteCmd.Flags().BoolVarP(&deleteFlags.force, "force", "f", false, "Delete even if the containers could not be taken down")
	rootCmd.AddCommand(deleteCmd)
}

// sharedDBServices are depended on by other services; deleting one breaks them.
var sharedDBServices = map[string]bool{"postgres": true, "mariadb": true, "redis": true}

func runDelete(_ *cobra.Command, args []string) error {
	root := configDir()
	return forEachService(root, args, func(name string) error { return deleteOne(root, name) })
}

func deleteOne(root, svcName string) error {
	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Delete: %s", svcName)))

	if sharedDBServices[svcName] {
		fmt.Printf("  %s  %s is a shared database — every service using it will break\n",
			styles.Warning.Render("!"), svcName)
	}
	if !deleteFlags.yes {
		ok, err := confirmToken(fmt.Sprintf("Type %q to delete it", svcName), svcName)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("not confirmed")
		}
	}

	// 1. Containers first. If they cannot be taken down, stop here: with the
	// compose file deleted, nothing could take them down afterwards.
	fmt.Printf("  %s  Removing containers…\n", styles.Muted.Render("→"))
	if err := stopAndRemoveService(root, svcName); err != nil {
		if !deleteFlags.force {
			return fmt.Errorf("taking %s down failed (%w) — nothing was deleted; fix that or pass --force", svcName, err)
		}
		fmt.Printf("  %s  down failed, continuing (--force): %v\n", styles.Warning.Render("!"), err)
	}

	// 2. Every network layer. Each layer's Disable removes its files before
	// reloading its daemon, so a stopped daemon still gets cleaned up — a
	// leftover torrc entry would bring the service back on the same onion if
	// one with this name is ever added again.
	fmt.Printf("  %s  Removing network config…\n", styles.Muted.Render("→"))
	_ = routing.DisablePrivate(root, svcName, nil)
	for _, ext := range []string{"cf", "i2p", "tor", "ygg"} {
		_ = configgen.RemoveAllPortFiles(root, ext, svcName)
		if layer, ok := extRegistry().Get(ext); ok {
			_ = layer.Disable(svcName)
		}
	}
	if l, err := i2pLayer(); err == nil {
		_ = l.RemoveTunnel(svcName)
	}
	if err := caddy.New(root).Reload(); err != nil {
		fmt.Printf("  %s  Caddy reload: %v\n", styles.Warning.Render("!"), err)
	}

	// 3. The service directory.
	svcDir := filepath.Join(root, "services", svcName)
	if err := os.RemoveAll(svcDir); err != nil {
		return fmt.Errorf("removing service directory: %w", err)
	}

	fmt.Printf("  %s  %s deleted (%s)\n\n", styles.Success.Render("✓"), styles.Bold.Render(svcName), svcDir)
	return nil
}
