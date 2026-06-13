package cmd

import (
	"os"

	"github.com/groot/homelab/internal/run"
	"github.com/spf13/cobra"
)

var imagesFlags struct {
	quiet bool
}

var imagesCmd = &cobra.Command{
	Use:   "images [service]",
	Short: "List Docker images used by services",
	Long: `List the Docker images used by created containers for a service or core stack
(equivalent to 'docker compose images').

  homelab images              # core stack images
  homelab images jellyfin     # one service images
  homelab images -q           # quiet mode (image IDs only)`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		imagesArgs := []string{"images"}
		if imagesFlags.quiet {
			imagesArgs = append(imagesArgs, "--quiet")
		}

		if len(args) > 0 {
			name := args[0]
			if err := validateService(root, name); err != nil {
				return err
			}
			return run.Default().DockerComposeEnv(
				run.ServiceComposeFile(root, name),
				buildEnv(root, name),
				imagesArgs...,
			)
		}

		composeFile := run.CoreComposeFile(root)
		if _, err := os.Stat(composeFile); err != nil {
			return err
		}
		return run.Default().DockerComposeEnv(
			composeFile,
			buildEnv(root, ""),
			imagesArgs...,
		)
	},
}

func init() {
	imagesCmd.Flags().BoolVarP(&imagesFlags.quiet, "quiet", "q", false, "Only display image IDs")
}
