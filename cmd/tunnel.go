package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

const cloudflaredContainer = "cloudflared"

var cfCmd = &cobra.Command{
	Use:   "cf",
	Short: "Manage Cloudflare Tunnel (cloudflared)",
	Long:  "Inspect cloudflared and manage DNS routes for public-internet service exposure.",
}

// ── status ────────────────────────────────────────────────────────────────────

var tunnelStatusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"ps"},
	Short:   "Show Cloudflare Tunnel status and connections",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		env := buildEnv(root, "")

		fmt.Printf("\n%s\n\n", styles.Header.Render("Cloudflare Tunnel"))

		if !extEnabled(root, "cf") {
			fmt.Printf("  %s  Cloudflare Tunnel not enabled.\n", styles.Warning.Render("!"))
			fmt.Printf("  Run %s to enable.\n\n",
				styles.Primary.Render("homelab ext enable cf"))
			return nil
		}

		if env["CF_TUNNEL_TOKEN"] == "" {
			fmt.Printf("  %s  CF_TUNNEL_TOKEN not configured.\n", styles.Warning.Render("!"))
			fmt.Printf("\n  Run %s to provide credentials.\n\n",
				styles.Primary.Render("homelab setup"))
			return nil
		}

		state := containerStatus(cloudflaredContainer)
		if state == containerStateRunning {
			fmt.Printf("  %s  cloudflared  %s\n", styles.Success.Render("✓"), styles.StateTag(state))
		} else {
			fmt.Printf("  %s  cloudflared  %s\n", styles.Err.Render("✗"), styles.StateTag(state))
			fmt.Printf("\n  Start with: %s\n\n", styles.Primary.Render("homelab start"))
			return nil
		}

		if tunnelName := env["CF_TUNNEL_NAME"]; tunnelName != "" {
			fmt.Printf("  %s  tunnel name  %s\n", styles.Muted.Render("↳"), styles.Bold.Render(tunnelName))
		}

		fmt.Printf("\n  %s\n", styles.Bold.Render("Active connections"))
		if err := run.Default().DockerExec(cloudflaredContainer, "cloudflared", "tunnel", "info"); err != nil {
			fmt.Printf("  %s  (run %s for details)\n",
				styles.Muted.Render("!"), styles.Primary.Render("homelab cf logs"))
		}
		fmt.Println()
		return nil
	},
}

// ── logs ──────────────────────────────────────────────────────────────────────

var tunnelLogsCmd = layerLogsCmd("cf",
	"Stream cloudflared logs")

// ── route ─────────────────────────────────────────────────────────────────────

var tunnelRouteCmd = &cobra.Command{
	Use:   "route",
	Short: "Manage Cloudflare DNS routes",
}

var tunnelRouteAddCmd = &cobra.Command{
	Use:   "add <service>",
	Short: "Add a Cloudflare DNS CNAME route for a service",
	Long: `Register a DNS CNAME so Cloudflare Tunnel serves the service publicly.

Requires CF_TUNNEL_NAME to be set (run 'homelab setup') and the cloudflared
container to be running ('homelab start').

After adding the DNS route, enable the public Caddy config:
  homelab enable <service> --cf`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		root := configDir()
		env := buildEnv(root, "")
		if !extEnabled(root, "cf") {
			return fmt.Errorf("cloudflare tunnel not enabled\n\n  Run %s to enable",
				styles.Primary.Render("homelab ext enable cf"))
		}
		if err := requireTunnelConfig(env); err != nil {
			return err
		}
		hostname := publicHostname(name, env)
		tunnelName := env["CF_TUNNEL_NAME"]
		fmt.Printf("%s Adding DNS route: %s → tunnel/%s\n",
			styles.Primary.Render("→"), styles.Bold.Render(hostname), tunnelName)
		if err := run.Default().DockerExec(cloudflaredContainer,
			"cloudflared", "tunnel", "route", "dns", tunnelName, hostname,
		); err != nil {
			return fmt.Errorf(
				"adding route %s: %w\n\n  Ensure CLOUDFLARE_API_TOKEN has DNS:Edit permissions", hostname, err)
		}
		fmt.Printf("%s DNS route added: %s\n", styles.Success.Render("✓"), styles.Bold.Render(hostname))
		fmt.Printf("  Enable public routing: %s\n\n",
			styles.Primary.Render(fmt.Sprintf("homelab enable %s --cf", name)))
		return nil
	},
}

var tunnelRouteRmCmd = &cobra.Command{
	Use:               "rm <service>",
	Short:             "Remove a Cloudflare DNS route for a service",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		root := configDir()
		env := buildEnv(root, "")
		if err := requireTunnelConfig(env); err != nil {
			return err
		}
		hostname := publicHostname(name, env)
		fmt.Printf("%s Removing DNS route: %s\n", styles.Warning.Render("→"), styles.Bold.Render(hostname))
		// cloudflared cannot delete DNS records — `route dns --overwrite-dns`,
		// which this used to run, re-creates the CNAME and then reported it
		// removed. The Cloudflare API can, with the token Caddy already uses
		// for DNS-01 (Zone:Read + DNS:Edit).
		n, err := deleteTunnelCNAME(cfAPIBase, env["CLOUDFLARE_API_TOKEN"], env["DOMAIN"], hostname)
		if err != nil {
			fmt.Printf("  %s  Could not remove the CNAME automatically: %v\n",
				styles.Warning.Render("!"), err)
			fmt.Printf("  Delete the CNAME record %s in the Cloudflare dashboard (DNS → Records), or:\n",
				styles.Bold.Render(hostname))
			fmt.Printf("    %s\n", styles.Muted.Render(fmt.Sprintf(
				`curl -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" "%s/zones/<zone_id>/dns_records?type=CNAME&name=%s"`,
				cfAPIBase, hostname)))
			fmt.Printf("    %s\n\n", styles.Muted.Render(fmt.Sprintf(
				`curl -X DELETE -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" "%s/zones/<zone_id>/dns_records/<record_id>"`,
				cfAPIBase)))
			return fmt.Errorf("DNS route %s not removed", hostname)
		}
		fmt.Printf("%s DNS route removed: %s (%d record(s))\n\n",
			styles.Success.Render("✓"), styles.Bold.Render(hostname), n)
		return nil
	},
}

// cfAPIBase is the Cloudflare v4 API root; a variable so tests can point it at
// an httptest server.
var cfAPIBase = "https://api.cloudflare.com/client/v4"

// cfResponse is the envelope every Cloudflare v4 API response shares.
type cfResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

// cfCall performs one Cloudflare API request and decodes its result into out.
func cfCall(method, url, token string, out any) error {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var body cfResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("%s %s: HTTP %d, undecodable body: %w", method, url, resp.StatusCode, err)
	}
	if !body.Success {
		msgs := make([]string, 0, len(body.Errors))
		for _, e := range body.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("cloudflare API: HTTP %d: %s", resp.StatusCode, strings.Join(msgs, "; "))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body.Result, out)
}

// deleteTunnelCNAME deletes the tunnel CNAME for hostname in zone and returns
// how many records it removed. Only CNAMEs pointing at a tunnel
// (*.cfargotunnel.com) are touched — a hand-made record of the same name is
// not ours to delete. Zero matches is an error: nothing was removed.
func deleteTunnelCNAME(apiBase, token, zone, hostname string) (int, error) {
	if token == "" {
		return 0, fmt.Errorf("CLOUDFLARE_API_TOKEN not configured")
	}
	if zone == "" {
		return 0, fmt.Errorf("DOMAIN not set")
	}
	var zones []struct {
		ID string `json:"id"`
	}
	if err := cfCall(http.MethodGet, apiBase+"/zones?name="+neturl.QueryEscape(zone), token, &zones); err != nil {
		return 0, fmt.Errorf("looking up zone %s: %w", zone, err)
	}
	if len(zones) == 0 {
		return 0, fmt.Errorf("zone %s not visible to CLOUDFLARE_API_TOKEN", zone)
	}
	zoneURL := apiBase + "/zones/" + zones[0].ID + "/dns_records"

	var records []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	if err := cfCall(http.MethodGet, zoneURL+"?type=CNAME&name="+neturl.QueryEscape(hostname), token, &records); err != nil {
		return 0, fmt.Errorf("listing CNAME %s: %w", hostname, err)
	}
	removed := 0
	for _, r := range records {
		if !strings.HasSuffix(r.Content, ".cfargotunnel.com") {
			continue
		}
		if err := cfCall(http.MethodDelete, zoneURL+"/"+r.ID, token, nil); err != nil {
			return removed, fmt.Errorf("deleting record %s: %w", r.ID, err)
		}
		removed++
	}
	if removed == 0 {
		return 0, fmt.Errorf("no tunnel CNAME named %s in zone %s", hostname, zone)
	}
	return removed, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func requireTunnelConfig(env map[string]string) error {
	var missing []string
	if env["CF_TUNNEL_TOKEN"] == "" {
		missing = append(missing, "CF_TUNNEL_TOKEN")
	}
	if env["CF_TUNNEL_NAME"] == "" {
		missing = append(missing, "CF_TUNNEL_NAME")
	}
	if len(missing) > 0 {
		return fmt.Errorf("cloudflare Tunnel not fully configured: %s missing\n\n  Run %s",
			strings.Join(missing, ", "), styles.Primary.Render("homelab setup"))
	}
	return nil
}

// publicHostname returns the public FQDN for a service (e.g. jellyfin.example.com).
// The label is the one the service's cf site block answers on — a --name or a
// declared subdomain, not necessarily the service name — so the DNS route and
// the Caddy route name the same host.
func publicHostname(svcName string, env map[string]string) string {
	return fmt.Sprintf("%s.%s", configgen.ServiceHost(configDir(), svcName), env["DOMAIN"])
}

func init() {
	tunnelRouteCmd.AddCommand(tunnelRouteAddCmd, tunnelRouteRmCmd)
	cfCmd.AddCommand(tunnelStatusCmd)
	cfCmd.AddCommand(tunnelLogsCmd)
	cfCmd.AddCommand(tunnelRouteCmd)
}
