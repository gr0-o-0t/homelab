package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	neturl "net/url"
	"os"
	"os/signal"
	"syscall"

	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/agent"
	"github.com/groot/homelab/internal/agentstate"
)

var agentFlags struct {
	root    string
	shimDir string
}

// agentCmd is the process the agent container runs on every PC. Hidden: it
// is plumbing, and refused remotely (remote.Internal).
var agentCmd = &cobra.Command{
	Use:    "agent",
	Short:  "Run the per-PC homelab agent (container entrypoint)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return fmt.Errorf("docker client: %w", err)
		}
		defer func() { _ = cli.Close() }()
		a := &agent.Agent{
			Root:      agentFlags.root,
			ShimDir:   agentFlags.shimDir,
			Version:   Version,
			Resolve:   ResolveCommand,
			API:       cli,
			Inspect:   func(ctx context.Context) (agent.NodeInfo, error) { return inspectNode(ctx, cli) },
			EnsureDNS: ensureWildcardA,
		}
		return a.Run(ctx)
	},
}

func init() {
	agentCmd.Flags().StringVar(&agentFlags.root, "root", agentstate.Root, "agent state volume")
	agentCmd.Flags().StringVar(&agentFlags.shimDir, "shim-dir", agent.DefaultShimDir, "mount point of the homelab-shim volume")
	rootCmd.AddCommand(agentCmd)
}

// inspectNode reads the engine's swarm membership: a manager leads, a worker
// serves local exec/logs, anything else is not in a swarm yet.
func inspectNode(ctx context.Context, cli *client.Client) (agent.NodeInfo, error) {
	info, err := cli.Info(ctx)
	if err != nil {
		return agent.NodeInfo{}, err
	}
	return nodeInfoFrom(info.Swarm, info.Name), nil
}

func nodeInfoFrom(s dswarm.Info, hostname string) agent.NodeInfo {
	n := agent.NodeInfo{Hostname: hostname}
	if s.LocalNodeState != dswarm.LocalNodeStateActive {
		return n
	}
	if s.ControlAvailable {
		n.Role = agent.RoleLeader
	} else {
		n.Role = agent.RoleWorker
	}
	return n
}

// ensureWildcardA makes name an A record to ip, DNS only (not proxied — a
// tailnet IP is useless behind Cloudflare's proxy), through the Cloudflare
// API with the token Caddy uses for DNS-01. It writes only when the record
// differs; other A records of that name (an old IP) are removed, and a
// record of another type is not ours to replace.
func ensureWildcardA(_ context.Context, token, zone, name string, ip netip.Addr) error {
	return ensureARecord(cfAPIBase, token, zone, name, ip.String())
}

func ensureARecord(apiBase, token, zone, name, ip string) error {
	zoneURL, err := cfRecordsURL(apiBase, token, zone)
	if err != nil {
		return err
	}
	var records []struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Content string `json:"content"`
		Proxied bool   `json:"proxied"`
	}
	if err := cfCall(http.MethodGet, zoneURL+"?name="+neturl.QueryEscape(name), token, nil, &records); err != nil {
		return fmt.Errorf("looking up %s: %w", name, err)
	}
	rec := map[string]any{
		"type": "A", "name": name, "content": ip, "proxied": false, "ttl": 1,
		"comment": "homelab: tailnet services (agent)",
	}
	for _, r := range records {
		if r.Type != "A" {
			return fmt.Errorf("a %s record for %s already exists (→ %s); not replacing it", r.Type, name, r.Content)
		}
	}
	// The record already right is kept; failing that the first is updated.
	for i, r := range records {
		if r.Content == ip && !r.Proxied {
			records[0], records[i] = records[i], records[0]
			break
		}
	}
	kept := false
	for _, r := range records {
		switch {
		case kept:
			// a second A record would round-robin clients to a stale IP
			if err := cfCall(http.MethodDelete, zoneURL+"/"+r.ID, token, nil, nil); err != nil {
				return fmt.Errorf("deleting extra A record %s: %w", r.ID, err)
			}
		case r.Content == ip && !r.Proxied:
			kept = true
		default:
			if err := cfCall(http.MethodPatch, zoneURL+"/"+r.ID, token, rec, nil); err != nil {
				return fmt.Errorf("updating %s: %w", name, err)
			}
			kept = true
		}
	}
	if kept {
		return nil
	}
	if err := cfCall(http.MethodPost, zoneURL, token, rec, nil); err != nil {
		return fmt.Errorf("creating %s: %w", name, err)
	}
	return nil
}
