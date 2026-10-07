package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/routing"
	"github.com/groot/homelab/internal/service"
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
	if err := validName(svcName); err != nil {
		return err
	}
	// --name becomes a Caddy site address: "*" would be a catch-all.
	if enableName != "" {
		if err := validName(enableName); err != nil {
			return fmt.Errorf("--name: %w", err)
		}
	}

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

	// Layers this run turns on, so a failure part-way can take them back off.
	// Without that, the blocks already written stay on disk and go live on the
	// next unrelated Caddy reload — a failed `--all` could make a service
	// public through the tunnel without anyone noticing.
	before := activeLayerSet(root, svcName)
	mgr := caddy.New(root)
	snap, err := mgr.Snapshot() // Caddy's dirs, restored if this run fails
	if err != nil {
		return err
	}
	var added []string
	rollback := func(cause error) error {
		_ = snap.Restore()
		for _, l := range added {
			if l == "ts" {
				_ = routing.DisablePrivate(root, svcName, nil)
				continue
			}
			_ = configgen.RemoveAllPortFiles(root, l, svcName)
			if layer, ok := extRegistry().Get(l); ok {
				_ = layer.Disable(svcName)
			}
		}
		if len(added) > 0 {
			fmt.Printf("  %s  rolled back: %s\n", styles.Warning.Render("!"), strings.Join(added, ", "))
		}
		return cause
	}

	// ── Private tailnet (always enabled) ───────────────────────────────
	if err := routing.EnablePrivate(root, svcName, enableName, enablePorts, nil); err != nil {
		return err
	}
	if !before["ts"] {
		added = append(added, "ts")
	}
	fmt.Printf("  %s  Private: %s.%s.%s\n",
		styles.Success.Render("✓"),
		displayName,
		"{$HOME_SUBDOMAIN}", "{$DOMAIN}",
	)

	// ── Extension layers ───────────────────────────────────────────────
	if !hasExts {
		fmt.Println()
		if err := mgr.ReloadOrRestore(snap); err != nil {
			return rollback(err)
		}
		return nil
	}

	for _, ext := range exts {
		if err := enableExtension(root, svcName, displayName, ext); err != nil {
			if !before[ext] {
				added = append(added, ext) // may be half-written
			}
			return rollback(fmt.Errorf("%s: %w", ext, err))
		}
		if !before[ext] {
			added = append(added, ext)
		}
		fmt.Printf("  %s  %s: enabled\n",
			styles.Success.Render("✓"),
			configgen.ExtensionLabel(ext),
		)
	}

	fmt.Println()
	if err := mgr.ReloadOrRestore(snap); err != nil {
		return rollback(err)
	}
	return nil
}

// activeLayerSet is the set of layers a service is currently exposed on.
func activeLayerSet(root, name string) map[string]bool {
	set := map[string]bool{}
	svcs, _ := service.Discover(root)
	for _, s := range svcs {
		if s.Name == name {
			for _, l := range s.ActiveLayers() {
				set[string(l)] = true
			}
		}
	}
	return set
}

// enableExtension configures one layer for a service: the layer's own config
// (tunnel, hidden service, forwarder) first, then the Caddy blocks.
//
// That order is deliberate. The layer is the half that can fail on
// environment — a root-owned tor key directory, a stopped daemon — and doing
// it second used to leave a Caddy block behind for a layer that was never
// configured, which `homelab status` then reported as an active exposure.
func enableExtension(root, svcName, displayName, ext string) error {
	blocks, err := configgen.Generate(configgen.Request{
		ServiceName: svcName,
		DisplayName: displayName,
		Extensions:  []string{ext},
		PortNames:   enablePorts,
		ConfigDir:   root,
	})
	if err != nil {
		return err
	}

	if layer, ok := extRegistry().Get(config.ResolveExtension(ext)); ok {
		if err := layer.Enable(svcName, displayName, layerServiceInfo(root, svcName), layerPorts(blocks)); err != nil {
			return err
		}
	}

	for _, b := range blocks {
		// Empty content means the network layer writes its own Caddy config
		// (ygg: the site address depends on a port only the layer knows), or
		// the port is not routable there (udp, or an explicit listen port on a
		// mesh layer).
		if b.Content == "" {
			continue
		}
		if err := configgen.WriteFile(root, b.Extension, svcName, b.PortName, b.Content); err != nil {
			return fmt.Errorf("writing %s config: %w", ext, err)
		}
	}
	return nil
}

// layerPorts converts generated blocks into the port list a layer records.
func layerPorts(blocks []configgen.CaddyBlock) []network.PortSelection {
	ports := make([]network.PortSelection, len(blocks))
	for i, b := range blocks {
		ports[i] = network.PortSelection{Name: b.PortName, Port: b.Port, Listen: b.Listen, Protocol: "tcp"}
	}
	return ports
}

// layerServiceInfo is the service's declared ports in the shape layers expect.
func layerServiceInfo(root, svcName string) network.ServiceInfo {
	cfgInfo, _ := configgen.LoadServiceInfo(root, svcName)
	info := network.ServiceInfo{
		Name:    svcName,
		Ports:   make(map[string]int, len(cfgInfo.Ports)),
		HasVars: cfgInfo.HasVars,
	}
	for k, v := range cfgInfo.Ports {
		info.Ports[k] = v.Port
	}
	return info
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
