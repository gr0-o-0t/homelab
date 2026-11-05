package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/swarm"
)

// clusterCmd is the leader's side of `homelab join`, which calls it through
// the remote CLI. Hidden plumbing: it refuses to run off a Swarm manager.
var clusterCmd = &cobra.Command{
	Use:    "cluster",
	Short:  "Leader-side plumbing for homelab join",
	Hidden: true,
}

var clusterJoinTokenCmd = &cobra.Command{
	Use:    "join-token",
	Short:  "Print the worker join token, manager address and tailnet auth key as JSON",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		return withSwarmAPI(func(api swarm.API) error {
			sm, err := openSecrets()
			if err != nil {
				return fmt.Errorf("opening the vault: %w", err)
			}
			tok, err := bootstrap.IssueJoinToken(cmdContext(), api, sm)
			if err != nil {
				return err
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(tok)
		})
	},
}

var clusterLabelCmd = &cobra.Command{
	Use:    "label <node-id> <core|worker>",
	Short:  "Set a node's homelab.role label",
	Hidden: true,
	Args:   cobra.ExactArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		return withSwarmAPI(func(api swarm.API) error {
			return bootstrap.LabelNode(cmdContext(), api, args[0], args[1])
		})
	},
}

// agentSeedFlags are agent-seed's non-secret cluster.yaml settings.
var agentSeedFlags struct {
	root, tailnet, leaderAddr string
}

// agentSeedCmd stores root secrets, read as JSON from stdin, in the agent's
// vault. init and join exec it inside the agent container (`docker exec -i
// homelab-agent homelab agent-seed`): the only way into the vault from
// outside, and it never takes a secret in argv.
var agentSeedCmd = &cobra.Command{
	Use:    "agent-seed",
	Short:  "Store root secrets from stdin JSON in the agent's vault",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		names, err := bootstrap.ApplySeed(agentSeedFlags.root, os.Stdin,
			bootstrap.SeedInfo{Tailnet: agentSeedFlags.tailnet, LeaderAddr: agentSeedFlags.leaderAddr})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(c.ErrOrStderr(), "stored %s\n", strings.Join(names, ", "))
		return nil
	},
}

func init() {
	clusterCmd.AddCommand(clusterJoinTokenCmd, clusterLabelCmd)
	rootCmd.AddCommand(clusterCmd)

	f := agentSeedCmd.Flags()
	f.StringVar(&agentSeedFlags.root, "root", agentstate.Root, "agent state volume")
	f.StringVar(&agentSeedFlags.tailnet, "tailnet", "", "tailnet MagicDNS suffix for cluster.yaml")
	f.StringVar(&agentSeedFlags.leaderAddr, "leader-addr", "", "Swarm manager address for cluster.yaml")
	rootCmd.AddCommand(agentSeedCmd)
}

// withSwarmAPI runs fn with a Docker client (or the test fake).
func withSwarmAPI(fn func(swarm.API) error) error {
	api, err := newSwarmAPI()
	if err != nil {
		return fmt.Errorf("connecting to docker: %w", err)
	}
	if c, ok := api.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	return fn(api)
}
