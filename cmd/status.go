package cmd

import (
	"fmt"
	"strings"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/diagnostics"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/edge"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:     "status [service]",
	Aliases: []string{"ps"},
	Short:   "Show homelab status overview",
	Long: `Display the status of the edge (the leader agent's Caddy, network extensions)
and every installed service.

With a service argument, show status for that specific service.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runStatus,
}

var statusCheckFlag bool

func runStatus(_ *cobra.Command, args []string) error {
	dir := configDir()
	cfgFile := rootConfigFile()
	cfg, _ := config.Load(cfgFile)
	env := buildEnv(dir, "")

	// Per-service status
	if len(args) > 0 {
		return runServiceStatus(dir, args[0], env)
	}

	fmt.Printf("\n%s\n\n", styles.Header.Render("Homelab Status"))

	// ── Edge ──────────────────────────────────────────────────────────────────
	// The leader agent runs Caddy and the tailnet node; cloudflared is a Swarm
	// service. Extensions show when enabled, or configured but not enabled.
	fmt.Printf("  %s\n", styles.Bold.Render("Edge"))
	fmt.Printf("  %s  %s  %s\n",
		styles.TableHeader.Render(styles.Width(styles.ColWidthName).Render("COMPONENT")),
		styles.TableHeader.Render(styles.Width(12).Render("STATE")),
		styles.TableHeader.Render(styles.Width(10).Render("CONFIG")),
	)
	fmt.Println(styles.Divider.Render("  " + strings.Repeat("─", styles.ColWidthName+12+10+4)))

	edgeRow := func(name, state, configTag string) {
		icon := styles.Warning.Render("!")
		switch state {
		case containerStateRunning:
			icon = styles.Success.Render("✓")
		case "not found", "not running":
			icon = styles.Err.Render("✗")
		}
		fmt.Printf("  %s  %s  %s\n",
			styles.Width(styles.ColWidthName).Render(icon+" "+styles.Bold.Render(truncate(name, styles.ColWidthName-3))),
			styles.Width(12).Render(styles.StateTag(state)),
			styles.Width(10).Render(configTag),
		)
	}
	caddyState := containerStateRunning
	if edge.Reachable(edge.AdminSocket(dir)) != nil {
		caddyState = "not running"
	}
	edgeRow("caddy", caddyState, styles.Muted.Render("always"))

	var hints []string
	for _, layer := range extRegistry().All() {
		ename := layer.Name()
		if ename == "ts" {
			continue // the tailnet node is the agent's, shown as caddy
		}
		enabled := cfg != nil && cfg.HasExtension(ename)
		configured := ename == cloudflaredLayer && env["CF_TUNNEL_TOKEN"] != ""
		state := layer.Status().ContainerState
		if !enabled && !configured && state == "not found" {
			continue
		}
		configTag := styles.Muted.Render("–")
		switch {
		case enabled:
			configTag = styles.Success.Render("enabled")
		case configured:
			configTag = styles.Warning.Render("configured")
			if state != containerStateRunning {
				hints = append(hints, fmt.Sprintf("Run %s to add %s",
					styles.Primary.Render("homelab ext enable "+ename), layer.Label()))
			}
		}
		edgeRow(layer.ContainerName(), state, configTag)
	}
	fmt.Println()

	if caddyState != containerStateRunning {
		hints = append(hints, fmt.Sprintf("The leader agent's Caddy is not answering — check %s",
			styles.Primary.Render("homelab caddy logs")))
	}
	if len(hints) > 0 {
		fmt.Printf("  %s\n", styles.Muted.Render(strings.Repeat("─", 50)))
		for _, h := range hints {
			fmt.Printf("  %s  %s\n", styles.Muted.Render("→"), h)
		}
		fmt.Println()
	}

	// ── Services ──────────────────────────────────────────────────────────────
	svcs, err := discoverServices(dir)
	if err != nil {
		svcs = nil
	}

	if len(svcs) == 0 {
		fmt.Printf("  %s  No services installed.\n", styles.Muted.Render("!"))
		fmt.Printf("  %s  Run %s from the catalog, or %s to scaffold a new one.\n\n",
			styles.Muted.Render("→"),
			styles.Primary.Render("homelab add <name>"),
			styles.Primary.Render("homelab new"))
		return nil
	}

	runningCount := 0
	layerCount := map[string]int{}
	for i := range svcs {
		s := &svcs[i]
		for _, l := range s.ActiveLayers() {
			layerCount[l]++
		}
		if s.Running > 0 {
			runningCount++
		}
	}
	privateCount, publicCount := layerCount["ts"], layerCount["cf"]
	torCount, i2pCount, yggCount := layerCount["tor"], layerCount["i2p"], layerCount["ygg"]

	summaryLine := fmt.Sprintf("%d installed / %d running", len(svcs), runningCount)
	if torCount > 0 || i2pCount > 0 || yggCount > 0 {
		summaryLine += fmt.Sprintf(" / ts:%d cf:%d tor:%d i2p:%d ygg:%d",
			privateCount, publicCount, torCount, i2pCount, yggCount)
	} else {
		summaryLine += fmt.Sprintf(" / %d private / %d public", privateCount, publicCount)
	}
	fmt.Printf("  %s  %s\n\n", styles.Bold.Render("Services"), styles.Muted.Render(summaryLine))

	// Table header
	fmt.Printf("  %s  %s  %s  %s  %s\n",
		styles.TableHeader.Render(styles.Width(styles.ColWidthName).Render("SERVICE")),
		styles.TableHeader.Render(styles.Width(12).Render("STATE")),
		styles.TableHeader.Render(styles.Width(styles.ColWidthPorts).Render("PORTS")),
		styles.TableHeader.Render(styles.Width(styles.ColWidthNetwork).Render("NETWORK")),
		styles.TableHeader.Render("URL"),
	)
	divLen := styles.ColWidthName + 12 + styles.ColWidthPorts + styles.ColWidthNetwork + 50 + 8
	fmt.Println(styles.Divider.Render("  " + strings.Repeat("─", divLen)))

	for i := range svcs {
		svc := &svcs[i]
		name := styles.Width(styles.ColWidthName).Render(truncate(svc.Name, styles.ColWidthName-1))

		// STATE column — merged state + health, aggregated across every
		// container the service has (not just whichever the API listed
		// first), from data discoverServices already fetched.
		stateCol := styles.Width(12).Render(mergedState(*svc, service.AggregateHealth(svc.Containers)))

		// PORTS column — always visible
		var portsStr string
		if len(svc.HostPorts) > 0 {
			portsStr = styles.Width(styles.ColWidthPorts).Render(truncate(strings.Join(svc.HostPorts, ", "), styles.ColWidthPorts-1))
		} else {
			portsStr = styles.Width(styles.ColWidthPorts).Render(styles.Muted.Render("–"))
		}

		// Collect active exposures with URLs
		type expRow struct {
			tag string
			url string
		}
		var rows []expRow

		// One row per layer the service is exposed on, each address resolved by
		// the layer that owns that network — see network.NetworkLayer.
		for _, name := range svc.ActiveLayers() {
			layer, ok := extRegistry().Get(name)
			if !ok {
				continue
			}
			for _, addr := range layer.ServiceAddresses(svc.Name, env) {
				rows = append(rows, expRow{layerTag(name), addressText(addr)})
			}
		}

		if len(rows) == 0 {
			// No exposures — single row with dash in NETWORK column
			fmt.Printf("  %s  %s  %s  %s  %s\n",
				name,
				stateCol,
				portsStr,
				styles.Width(styles.ColWidthNetwork).Render(styles.Muted.Render("–")),
				"",
			)
		} else {
			// First exposure on summary row
			fmt.Printf("  %s  %s  %s  %s  %s\n",
				name,
				stateCol,
				portsStr,
				styles.Width(styles.ColWidthNetwork).Render(rows[0].tag),
				styles.Muted.Render(rows[0].url),
			)
			// Remaining exposures as sub-rows (empty SERVICE/STATE/PORTS)
			for _, r := range rows[1:] {
				fmt.Printf("  %s  %s  %s  %s  %s\n",
					styles.Width(styles.ColWidthName).Render(""),
					styles.Width(12).Render(""),
					styles.Width(styles.ColWidthPorts).Render(""),
					styles.Width(styles.ColWidthNetwork).Render(r.tag),
					styles.Muted.Render(r.url),
				)
			}
		}
	}

	// ── Diagnostics (--check) ─────────────────────────────────────────────────────
	if statusCheckFlag {
		dc, _ := docker.New()
		if dc != nil {
			defer func() { _ = dc.Close() }()
		}
		var pass = true
		fmt.Println()
		groups := []diagnostics.CheckGroup{
			diagnostics.RunConfigChecks(cfgFile),
			diagnostics.RunInfraChecks(dc),
		}
		for _, g := range groups {
			if len(g.Results) == 0 {
				continue
			}
			fmt.Printf("\n  %s\n", styles.Bold.Render(g.Title))
			for _, r := range g.Results {
				switch r.Status {
				case diagnostics.StatusPass:
					fmt.Printf("  %s  %s\n", styles.Success.Render("✓"), r.Message)
				case diagnostics.StatusFail:
					fmt.Printf("  %s  %s\n", styles.Err.Render("✗"), r.Message)
					pass = false
				case diagnostics.StatusWarn:
					fmt.Printf("  %s  %s\n", styles.Warning.Render("!"), r.Message)
				}
			}
		}
		fmt.Println()
		if !pass {
			fmt.Printf("  %s\n", styles.Err.Render("Some checks failed."))
		}
	}

	return nil
}

// runServiceStatus shows status for a single service with container detail table.

func init() {
	statusCmd.Flags().BoolVar(&statusCheckFlag, "check", false, "Run health checks inline")
}
