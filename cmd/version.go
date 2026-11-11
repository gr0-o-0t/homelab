package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/bootstrap"
)

// Version is injected at build time via -ldflags.
var Version = "dev"

var versionFlags struct{ agent bool }

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the homelab version",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(Version)
		if versionFlags.agent {
			if id := agentBootID(); id != "" {
				fmt.Println("boot " + id)
			}
		}
	},
}

// agentBootID is the ID of this node's agent container: `homelab upgrade`
// asks an agent for it (`version --agent`) before and after replacing it,
// and only a changed one proves the replacement answers. "" when there is
// no agent container here.
func agentBootID() string {
	api, err := newSwarmAPI()
	if err != nil {
		return ""
	}
	if c, ok := api.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	insp, err := api.ContainerInspect(cmdContext(), bootstrap.AgentContainer)
	if err != nil || insp.ContainerJSONBase == nil {
		return ""
	}
	return insp.ID
}

func init() {
	versionCmd.Flags().BoolVar(&versionFlags.agent, "agent", false, "also print this node's agent container ID (upgrade plumbing)")
	_ = versionCmd.Flags().MarkHidden("agent")
	rootCmd.AddCommand(versionCmd)
}
