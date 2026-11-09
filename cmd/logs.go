package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs [service]",
	Short: "Print a service's logs, or the leader agent's (like docker compose logs)",
	Long: `Print logs for a service: every container of its stack, from whichever node
runs it, each line prefixed with its compose key. Without a service, print the
leader agent's logs (the agent, its Caddy and its tailnet node).

  homelab logs jellyfin             # print and exit
  homelab logs -f -n 100 jellyfin   # follow, starting from the last 100 lines
  homelab logs -c server immich     # one container of the stack
  homelab logs --tui jellyfin       # full-screen scrolling viewer
  homelab logs -f                   # the leader agent`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return runServiceLogs(cmd, args)
		}
		if logsFlags.container != "" || logsFlags.tui {
			return fmt.Errorf("--container and --tui need a service")
		}
		return agentLogs(cmd.OutOrStdout(), cmd.ErrOrStderr(), agentLogsFlags())
	},
}

// agentLogsFlags maps homelab's logs flags onto `docker logs`.
func agentLogsFlags() []string {
	var a []string
	if logsFlags.follow {
		a = append(a, "--follow")
	}
	if logsFlags.timestamps {
		a = append(a, "--timestamps")
	}
	for _, kv := range [][2]string{{"--tail", logsFlags.tail}, {"--since", logsFlags.since}, {"--until", logsFlags.until}} {
		if kv[1] != "" {
			a = append(a, kv[0], kv[1])
		}
	}
	return a
}

func init() {
	f := logsCmd.Flags()
	f.BoolVarP(&logsFlags.follow, "follow", "f", false, "Follow log output")
	f.BoolVarP(&logsFlags.timestamps, "timestamps", "t", false, "Show timestamps")
	f.StringVarP(&logsFlags.tail, "tail", "n", "", `Number of lines to show from the end (e.g. "100", "all")`)
	f.StringVar(&logsFlags.since, "since", "", `Show logs since timestamp or relative duration (e.g. "30m", "2h")`)
	f.StringVar(&logsFlags.until, "until", "", `Show logs before timestamp or relative duration (agent logs only)`)
	f.BoolVar(&logsFlags.tui, "tui", false, "Open the full-screen log viewer")
	f.StringVarP(&logsFlags.container, "container", "c", "", "Only this container of the service: a compose key or container_name")
}
