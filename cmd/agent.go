package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/groot/homelab/internal/bootstrap"
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

var selfReplaceFlags struct {
	image    string
	pullOnly bool
}

// agentSelfReplaceCmd is how the leader's `homelab upgrade` reaches a
// worker: a worker agent runs it for the leader (remote AllowInternal),
// pulling the image (--pull-only) or handing its own replacement to a helper
// container. The leader never runs it: it replaces itself in-process, as
// upgrade's last step.
var agentSelfReplaceCmd = &cobra.Command{
	Use:    "self-replace",
	Short:  "Replace this worker's agent container with one on --image (upgrade plumbing)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if selfReplaceFlags.image == "" {
			return errors.New("--image is required")
		}
		api, err := newSwarmAPI()
		if err != nil {
			return fmt.Errorf("connecting to docker: %w", err)
		}
		if c, ok := api.(io.Closer); ok {
			defer func() { _ = c.Close() }()
		}
		ctx := cmdContext()
		info, err := api.Info(ctx)
		if err != nil {
			return fmt.Errorf("asking Docker for this node's swarm role: %w", err)
		}
		if info.Swarm.ControlAvailable {
			return errors.New("refusing on the leader: its agent is replaced only as the last step of `homelab upgrade`")
		}
		if selfReplaceFlags.pullOnly {
			if err := bootstrap.EnsureImage(ctx, api, selfReplaceFlags.image, os.Stdout); err != nil {
				return err
			}
			fmt.Printf("%s is on this node\n", selfReplaceFlags.image)
			return nil
		}
		return bootstrap.SelfReplace(ctx, api, selfReplaceFlags.image, os.Stdout)
	},
}

// agentReplaceHelperCmd is the helper container's process: it recreates the
// agent container from the spec in its environment, then exits (the
// container removes itself).
var agentReplaceHelperCmd = &cobra.Command{
	Use:    "replace-helper",
	Short:  "Recreate the agent container from $" + bootstrap.ReplaceSpecEnv + " (upgrade plumbing)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		var spec bootstrap.ReplaceSpec
		if err := json.Unmarshal([]byte(os.Getenv(bootstrap.ReplaceSpecEnv)), &spec); err != nil {
			return fmt.Errorf("reading $%s: %w", bootstrap.ReplaceSpecEnv, err)
		}
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return fmt.Errorf("docker client: %w", err)
		}
		defer func() { _ = cli.Close() }()
		// Not cancelled by a signal: stopping halfway would leave no agent.
		return bootstrap.RunReplace(context.Background(), cli, spec, bootstrap.Sleep)
	},
}

func init() {
	agentCmd.Flags().StringVar(&agentFlags.root, "root", agentstate.Root, "agent state volume")
	agentCmd.Flags().StringVar(&agentFlags.shimDir, "shim-dir", agent.DefaultShimDir, "mount point of the homelab-shim volume")
	agentSelfReplaceCmd.Flags().StringVar(&selfReplaceFlags.image, "image", "", "the agent image to run")
	agentSelfReplaceCmd.Flags().BoolVar(&selfReplaceFlags.pullOnly, "pull-only", false, "only make the image available on this node")
	agentCmd.AddCommand(agentSelfReplaceCmd, agentReplaceHelperCmd)
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
// differs; other A records of that name (an old IP) are removed, a CNAME
// there is not ours to replace, and other types (AAAA, TXT) are left alone.
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
	// Only a CNAME conflicts with an A record (it must stand alone); AAAA,
	// TXT and the like coexist and are not ours, so they are left alone.
	as := records[:0]
	for _, r := range records {
		switch r.Type {
		case "A":
			as = append(as, r)
		case "CNAME":
			return fmt.Errorf("a CNAME record for %s already exists (→ %s); not replacing it", name, r.Content)
		}
	}
	records = as
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
