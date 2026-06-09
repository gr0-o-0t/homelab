package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

// enableCmd represents the unified `homelab enable` command.
var enableCmd = &cobra.Command{
	Use:   "enable <service>",
	Short: "Enable a service with network exposure layers",
	Long: `Enable a service, writing Caddy routes and network extension configs.

By default the service is exposed on your private tailnet
(*.{HOME_SUBDOMAIN}.{DOMAIN}). Additional network layers are enabled
with extension flags:

  --cf    expose via Cloudflare Tunnel (public internet)
  --i2p   expose as I2P eepsite (.i2p)
  --tor   expose as Tor onion service (.onion)
  --ygg   expose on Yggdrasil IPv6 mesh

Port selection defaults to all ports declared in the service's config.yaml.
Use --ports to expose specific named ports only.

Examples:
  homelab enable gitea                      # private tailnet only
  homelab enable gitea --cf                 # private + Cloudflare
  homelab enable gitea --cf --i2p           # private + CF + I2P
  homelab enable gitea --ports=web,ssh --cf # specific ports + CF
  homelab enable gitea --name=dev-gitea     # custom display name`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runEnable,
}

var (
	enableCf      bool
	enableI2P     bool
	enableTor     bool
	enableYgg     bool
	enableAllExts bool

	enableName  string
	enablePorts []string
)

func init() {
	enableCmd.Flags().BoolVar(&enableCf, "cf", false, "Expose via Cloudflare Tunnel")
	enableCmd.Flags().BoolVar(&enableI2P, "i2p", false, "Expose as I2P eepsite")
	enableCmd.Flags().BoolVar(&enableTor, "tor", false, "Expose as Tor onion service")
	enableCmd.Flags().BoolVar(&enableYgg, "ygg", false, "Expose on Yggdrasil mesh")
	enableCmd.Flags().BoolVar(&enableAllExts, "all", false, "Enable all available extensions")
	enableCmd.Flags().StringVar(&enableName, "name", "", "Custom display name (subdomain)")
	enableCmd.Flags().StringSliceVar(&enablePorts, "ports", nil, "Specific named ports to expose (comma-separated)")
	rootCmd.AddCommand(enableCmd)
}

func runEnable(cmd *cobra.Command, args []string) error {
	svcName := args[0]
	root := configDir()

	exts := buildExtensionList()
	hasExts := len(exts) > 0

	fmt.Printf("\n%s\n\n", styles.Header.Render(fmt.Sprintf("Enable: %s", svcName)))

	// ── Service info ───────────────────────────────────────────────────
	info, err := configgen.LoadServiceInfo(root, svcName)
	if err != nil {
		return fmt.Errorf("reading service config: %w", err)
	}
	if !info.HasVars {
		fmt.Printf("  %s  No config.yaml found for %s\n",
			styles.Warning.Render("!"), svcName)
		fmt.Printf("  %s  Run %s first\n\n",
			styles.Muted.Render("→"),
			styles.Primary.Render(fmt.Sprintf("homelab setup %s", svcName)))
		return nil
	}

	displayName := svcName
	if enableName != "" {
		displayName = enableName
	}

	// ── Private tailnet (always enabled) ───────────────────────────────
	if err := enablePrivate(root, svcName, info); err != nil {
		return err
	}
	fmt.Printf("  %s  Private: %s.%s.%s\n",
		styles.Success.Render("✓"),
		displayName,
		"{$HOME_SUBDOMAIN}", "{$DOMAIN}",
	)

	// ── Extension layers ───────────────────────────────────────────────
	if !hasExts {
		fmt.Println()
		return caddyReload()
	}

	for _, ext := range exts {
		if err := enableExtension(root, svcName, displayName, ext); err != nil {
			return fmt.Errorf("%s: %w", ext, err)
		}
		fmt.Printf("  %s  %s: enabled\n",
			styles.Success.Render("✓"),
			configgen.ExtensionLabel(ext),
		)
	}

	fmt.Println()
	return caddyReload()
}

func enablePrivate(root, svcName string, info configgen.ServiceInfo) error {
	// Check if custom caddy.conf exists → use symlink (legacy compatibility)
	customPath := root + "/services/" + svcName + "/caddy.conf"
	if _, err := os.Stat(customPath); err == nil {
		return caddy.New(root).Enable(svcName)
	}

	if len(info.Ports) == 0 {
		return fmt.Errorf("no ports defined in config.yaml and no caddy.conf found for %s", svcName)
	}

	req := configgen.Request{
		ServiceName: svcName,
		DisplayName: enableName,
		Extensions:  []string{"private"},
		PortNames:   enablePorts,
		ConfigDir:   root,
	}
	blocks, err := configgen.Generate(req)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if err := configgen.WriteFile(root, b.Extension, svcName, b.PortName, b.Content); err != nil {
			return fmt.Errorf("writing private config: %w", err)
		}
	}
	return nil
}

func enableExtension(root, svcName, displayName, ext string) error {
	req := configgen.Request{
		ServiceName: svcName,
		DisplayName: displayName,
		Extensions:  []string{ext},
		PortNames:   enablePorts,
		ConfigDir:   root,
	}
	blocks, err := configgen.Generate(req)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if err := configgen.WriteFile(root, b.Extension, svcName, b.PortName, b.Content); err != nil {
			return fmt.Errorf("writing %s config: %w", ext, err)
		}
	}

	// Network extension-specific tunnel config
	switch ext {
	case i2pContainer:
		return enableI2PLayer(root, displayName, blocks)
	case torContainer:
		return enableTorLayer(root, displayName, blocks)
	case "ygg":
		return enableYggLayer(root, displayName, blocks)
	}
	return nil
}

func buildExtensionList() []string {
	if enableAllExts {
		return []string{"cf", "i2p", "tor", "ygg"}
	}
	var exts []string
	if enableCf {
		exts = append(exts, "cf")
	}
	if enableI2P {
		exts = append(exts, "i2p")
	}
	if enableTor {
		exts = append(exts, "tor")
	}
	if enableYgg {
		exts = append(exts, "ygg")
	}
	sort.Strings(exts)
	return exts
}

func caddyReload() error {
	mgr := caddy.New(configDir())
	return mgr.Reload()
}

// ── Extension-specific enable helpers ───────────────────────────────────────

func enableI2PLayer(root, displayName string, blocks []configgen.CaddyBlock) error {
	for _, b := range blocks {
		port := extractPortFromBlock(b.Content, displayName)
		if port == "" {
			continue
		}
		if err := AppendI2PTunnel(root, displayName, port); err != nil {
			return err
		}
	}
	if containerStatus(i2pContainer) == containerStateRunning {
		return ReloadI2pd()
	}
	return nil
}

func enableTorLayer(root, displayName string, blocks []configgen.CaddyBlock) error {
	for _, b := range blocks {
		port := extractPortFromBlock(b.Content, displayName)
		if port == "" {
			continue
		}
		if err := AppendTorService(root, displayName, port); err != nil {
			return err
		}
	}
	if containerStatus(torContainer) == containerStateRunning {
		return ReloadTor()
	}
	return nil
}

func enableYggLayer(root, displayName string, blocks []configgen.CaddyBlock) error {
	for _, b := range blocks {
		port := extractPortFromBlock(b.Content, displayName)
		if port == "" {
			continue
		}
		if err := AppendYggForwarder(root, displayName, port); err != nil {
			return err
		}
	}
	if containerStatus(yggContainer) == containerStateRunning {
		return RestartYgg()
	}
	return nil
}

// extractPortFromBlock parses a generated Caddy config to find the target port.
// Looks for the first "reverse_proxy <name>:<N>" pattern in the block content.
func extractPortFromBlock(content, svcName string) string {
	prefix := "reverse_proxy " + svcName + ":"
	idx := strings.Index(content, prefix)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(prefix):]
	end := strings.IndexAny(rest, " \t\n\r}")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
