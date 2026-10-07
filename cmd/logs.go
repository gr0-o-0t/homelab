package cmd

import (
	"github.com/groot/homelab/internal/run"
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs [service]",
	Short: "Print core stack or service logs (like docker compose logs)",
	Long: `Print logs for a service, or for the core stack when no service is given.
Flags are the docker compose logs flags.

  homelab logs jellyfin             # print and exit
  homelab logs -f -n 100 jellyfin   # follow, starting from the last 100 lines
  homelab logs --tui jellyfin       # full-screen scrolling viewer
  homelab logs -f                   # core stack`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return runServiceLogs(cmd, args)
		}
		dir := configDir()
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			buildEnv(dir, ""),
			withProfiles(dir, logsArgs()...)...,
		)
	},
}

func init() {
	f := logsCmd.Flags()
	f.BoolVarP(&logsFlags.follow, "follow", "f", false, "Follow log output")
	f.BoolVarP(&logsFlags.timestamps, "timestamps", "t", false, "Show timestamps")
	f.StringVarP(&logsFlags.tail, "tail", "n", "", `Number of lines to show from the end (e.g. "100", "all")`)
	f.StringVar(&logsFlags.since, "since", "", `Show logs since timestamp or relative duration (e.g. "30m", "2h")`)
	f.StringVar(&logsFlags.until, "until", "", `Show logs before timestamp or relative duration`)
	f.BoolVar(&logsFlags.tui, "tui", false, "Open the full-screen log viewer")
}
