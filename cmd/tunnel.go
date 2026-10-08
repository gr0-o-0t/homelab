package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

Creates a proxied CNAME <host>.<DOMAIN> → <tunnel-id>.cfargotunnel.com through
the Cloudflare API, using CLOUDFLARE_API_TOKEN (Zone:Read + DNS:Edit — the token
Caddy already uses for DNS-01) and the tunnel id inside CF_TUNNEL_TOKEN.
'homelab enable <service> --cf' does this itself; this command is for adding
the record by hand. An existing tunnel record is left as it is.`,
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
		hostname, created, err := addTunnelRoute(env, name)
		if err != nil {
			return fmt.Errorf("adding DNS route %s: %w", hostname, err)
		}
		if created {
			fmt.Printf("%s DNS route added: %s → tunnel\n", styles.Success.Render("✓"), styles.Bold.Render(hostname))
		} else {
			fmt.Printf("%s DNS route already present: %s\n", styles.Success.Render("✓"), styles.Bold.Render(hostname))
		}
		fmt.Println()
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
func cfCall(method, url, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var res cfResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("%s %s: HTTP %d, undecodable body: %w", method, url, resp.StatusCode, err)
	}
	if !res.Success {
		msgs := make([]string, 0, len(res.Errors))
		for _, e := range res.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("cloudflare API: HTTP %d: %s", resp.StatusCode, strings.Join(msgs, "; "))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res.Result, out)
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
	zoneURL, err := cfRecordsURL(apiBase, token, zone)
	if err != nil {
		return 0, err
	}

	var records []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	if err := cfCall(http.MethodGet, zoneURL+"?type=CNAME&name="+neturl.QueryEscape(hostname), token, nil, &records); err != nil {
		return 0, fmt.Errorf("listing CNAME %s: %w", hostname, err)
	}
	removed := 0
	for _, r := range records {
		if !strings.HasSuffix(r.Content, ".cfargotunnel.com") {
			continue
		}
		if err := cfCall(http.MethodDelete, zoneURL+"/"+r.ID, token, nil, nil); err != nil {
			return removed, fmt.Errorf("deleting record %s: %w", r.ID, err)
		}
		removed++
	}
	if removed == 0 {
		return 0, fmt.Errorf("no tunnel CNAME named %s in zone %s", hostname, zone)
	}
	return removed, nil
}

// cfRecordsURL resolves zone to its dns_records endpoint.
func cfRecordsURL(apiBase, token, zone string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("CLOUDFLARE_API_TOKEN not configured")
	}
	if zone == "" {
		return "", fmt.Errorf("DOMAIN not set")
	}
	var zones []struct {
		ID string `json:"id"`
	}
	if err := cfCall(http.MethodGet, apiBase+"/zones?name="+neturl.QueryEscape(zone), token, nil, &zones); err != nil {
		return "", fmt.Errorf("looking up zone %s: %w", zone, err)
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("zone %s not visible to CLOUDFLARE_API_TOKEN", zone)
	}
	return apiBase + "/zones/" + zones[0].ID + "/dns_records", nil
}

// ensureTunnelCNAME creates hostname → <tunnelID>.cfargotunnel.com as a
// proxied CNAME, which is what routes a public name into the tunnel. A record
// already pointing there is left alone (created=false); any other record with
// that name is not ours to replace, so it is an error.
func ensureTunnelCNAME(apiBase, token, zone, hostname, tunnelID string) (created bool, err error) {
	zoneURL, err := cfRecordsURL(apiBase, token, zone)
	if err != nil {
		return false, err
	}
	target := tunnelID + ".cfargotunnel.com"
	var records []struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := cfCall(http.MethodGet, zoneURL+"?name="+neturl.QueryEscape(hostname), token, nil, &records); err != nil {
		return false, fmt.Errorf("looking up %s: %w", hostname, err)
	}
	for _, r := range records {
		if r.Type == "CNAME" && strings.EqualFold(r.Content, target) {
			return false, nil
		}
	}
	if len(records) > 0 {
		r := records[0]
		return false, fmt.Errorf("a %s record for %s already exists (→ %s); not replacing it", r.Type, hostname, r.Content)
	}
	rec := map[string]any{
		"type": "CNAME", "name": hostname, "content": target,
		"proxied": true, "ttl": 1, "comment": "homelab: Cloudflare Tunnel route",
	}
	if err := cfCall(http.MethodPost, zoneURL, token, rec, nil); err != nil {
		return false, fmt.Errorf("creating CNAME %s: %w", hostname, err)
	}
	return true, nil
}

// tunnelIDFromToken returns the tunnel UUID carried in a cloudflared tunnel
// token — base64 JSON {"a": account tag, "t": tunnel id, "s": secret} — so the
// DNS target can be built without a cert.pem or an extra API permission.
func tunnelIDFromToken(tok string) (string, error) {
	tok = strings.TrimSpace(tok)
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(tok); err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("CF_TUNNEL_TOKEN is not a tunnel token: %w", err)
	}
	var t struct {
		Tunnel string `json:"t"`
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.Tunnel == "" {
		return "", fmt.Errorf("CF_TUNNEL_TOKEN carries no tunnel id")
	}
	return t.Tunnel, nil
}

// addTunnelRoute makes svc's public hostname resolve into the tunnel. It
// returns the hostname even on error, for messages.
func addTunnelRoute(env map[string]string, svc string) (hostname string, created bool, err error) {
	hostname = publicHostname(svc, env)
	if err := requireTunnelConfig(env); err != nil {
		return hostname, false, err
	}
	tid, err := tunnelIDFromToken(env["CF_TUNNEL_TOKEN"])
	if err != nil {
		return hostname, false, err
	}
	created, err = ensureTunnelCNAME(cfAPIBase, env["CLOUDFLARE_API_TOKEN"], env["DOMAIN"], hostname, tid)
	return hostname, created, err
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
