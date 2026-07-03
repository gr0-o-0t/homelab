package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:     "delete <service>",
	Aliases: []string{"rm"},
	Short:   "Remove a service entirely",
	Long: `Remove a service from homelab: disable all network exposure, stop
containers, and delete the service directory.

  homelab delete jellyfin
  homelab rm jellyfin`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runDelete,
}

func init() {
	rootCmd.AddCommand(deleteCmd)
}

func runDelete(_ *cobra.Command, args []string) error {
	svcName := args[0]
	root := configDir()

	if err := validateService(root, svcName); err != nil {
		return err
	}

	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Delete: %s", svcName)))

	// 1. Disable all network exposure
	fmt.Printf("  %s  Removing network config…\n", styles.Muted.Render("→"))

	// Private tailnet
	_ = configgen.RemoveAllPortFiles(root, "private", svcName)
	_ = caddy.New(root).Disable(svcName)

	// Extension layers
	_ = configgen.RemoveAllPortFiles(root, "cf", svcName)
	_ = configgen.RemoveAllPortFiles(root, "i2p", svcName)
	_ = configgen.RemoveAllPortFiles(root, "tor", svcName)
	_ = configgen.RemoveAllPortFiles(root, "ygg", svcName)

	// Remove I2P tunnel
	_ = RemoveI2PTunnel(root, svcName)
	// Remove Tor service
	_ = RemoveTorService(root, svcName)
	// Remove Ygg forwarder
	_ = RemoveYggForwarder(root, svcName)

	// Reload Caddy
	if err := caddy.New(root).Reload(); err != nil {
		fmt.Printf("  %s  Caddy reload: %v\n", styles.Warning.Render("!"), err)
	}

	// Reload extension daemons
	if containerStatus(i2pContainer) == containerStateRunning {
		_ = ReloadI2pd()
	}
	if containerStatus(torContainer) == containerStateRunning {
		_ = ReloadTor()
	}
	if containerStatus(yggContainer) == containerStateRunning {
		_ = RestartYgg()
	}

	// 2. Stop service containers
	fmt.Printf("  %s  Stopping containers…\n", styles.Muted.Render("→"))
	_ = stopAndRemoveService(root, svcName)

	// 3. Remove service directory
	svcDir := filepath.Join(root, "services", svcName)
	if err := os.RemoveAll(svcDir); err != nil {
		return fmt.Errorf("removing service directory: %w", err)
	}

	fmt.Printf("  %s  %s deleted (%s)\n",
		styles.Success.Render("✓"),
		styles.Bold.Render(svcName),
		svcDir)
	fmt.Println()
	return nil
}
