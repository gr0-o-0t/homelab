package cmd

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/run"
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
Use --ports to expose specific named ports only. --name and --ports are
remembered (services/<name>/exposure.yaml): enabling another layer later
keeps them unless given again; --name=<service> or a --ports listing every
port resets them.

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
	enableLayerFlags = map[string]*bool{} // layer name → its --<flag>
	enableAllExts    bool

	enableName  string
	enablePorts []string
)

func init() {
	for _, l := range layers.Static() {
		if l.Flag() != "" {
			enableLayerFlags[l.Name()] = enableCmd.Flags().Bool(l.Flag(), false, "Expose via "+l.Label())
		}
	}
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

	if err := requireSitesLayout(root, svcName); err != nil {
		return err
	}

	// --name and --ports replace what the service was enabled with; without
	// them the stored ones are kept (see caddy.Manager.Enable).
	var name *string
	if cmd.Flags().Changed("name") {
		name = &enableName
	}
	var ports []string
	if cmd.Flags().Changed("ports") {
		ports = enablePorts
	}

	mgr, _, explain := quietCaddy(root)
	ls := selectedEnableLayers()
	if undone, err := mgr.Enable(extRegistry(), svcName, ls, name, ports); err != nil {
		if len(undone) > 0 {
			fmt.Printf("  %s  rolled back: %s\n", styles.Warning.Render("!"), strings.Join(undone, ", "))
		}
		return explain(err)
	}

	host := configgen.ServiceHost(root, svcName)
	for _, l := range ls {
		if l.Flag() == "" {
			fmt.Printf("  %s  Private: %s.%s.%s\n",
				styles.Success.Render("✓"), host, "{$HOME_SUBDOMAIN}", "{$DOMAIN}")
		} else {
			fmt.Printf("  %s  %s: enabled\n", styles.Success.Render("✓"), l.Label())
		}
	}
	fmt.Println()
	return nil
}

// selectedEnableLayers is the private layer plus every layer whose flag was
// given (or all of them with --all), in registry order.
func selectedEnableLayers() []network.NetworkLayer {
	var out []network.NetworkLayer
	for _, l := range extRegistry().All() {
		if l.Flag() == "" || enableAllExts || *enableLayerFlags[l.Name()] {
			out = append(out, l)
		}
	}
	return out
}

func caddyReload() error {
	mgr, _, explain := quietCaddy(configDir())
	return explain(mgr.Reload())
}

// quietCaddy returns a Caddy manager whose docker output — validate and reload
// emit a screenful of JSON log lines — is captured instead of streamed, the
// runner it uses (for routing calls that reload through their own manager),
// and explain, which attaches the tail of that output to an error. On success
// the logs are noise; on failure they are the explanation.
func quietCaddy(root string) (*caddy.Manager, *run.Commander, func(error) error) {
	var buf bytes.Buffer
	r := &run.Commander{Stdout: &buf, Stderr: &buf}
	explain := func(err error) error {
		if err == nil || buf.Len() == 0 {
			return err
		}
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) > 8 {
			lines = lines[len(lines)-8:]
		}
		return fmt.Errorf("%w\n%s", err, strings.Join(lines, "\n"))
	}
	return caddy.NewWithRunner(root, r), r, explain
}
