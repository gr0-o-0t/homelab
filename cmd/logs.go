package cmd

import (
	"github.com/groot/homelab/internal/run"
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:               "logs [service]",
	Short:             "Tail core stack or service logs",
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := configDir()
		// With a service arg, tail that service's logs.
		if len(args) > 0 {
			return runServiceLogs(cmd, args)
		}
		// No arg → core stack logs.
		env := buildEnv(dir, "")
		return run.Default().DockerComposeEnv(
			run.CoreComposeFile(dir),
			env,
			withProfiles(dir, "logs", "-f")...,
		)
	},
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFlags.follow, "follow", "f", false, "Follow log output")
	logsCmd.Flags().StringVar(&logsFlags.tail, "tail", "", `Number of lines to show from the end (e.g. "100", "all")`)
	logsCmd.Flags().StringVar(&logsFlags.since, "since", "", `Show logs since timestamp or relative duration (e.g. "30m", "2h", "2006-01-02T15:04:05Z")`)
}
