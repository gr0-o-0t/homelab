package cmd

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"text/tabwriter"

	"github.com/groot/homelab/internal/swarm"
	"github.com/spf13/cobra"
)

var imagesFlags struct {
	quiet bool
}

var imagesCmd = &cobra.Command{
	Use:   "images <service>",
	Short: "List the Docker images a service deploys",
	Long: `List the image of every container of a service's stack, as its compose file
resolves them.

  homelab images jellyfin     # compose key and image
  homelab images -q immich    # image references only`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, name := configDir(), args[0]
		if err := validateService(root, name); err != nil {
			return err
		}
		p, err := swarm.LoadProject(filepath.Join(root, "services", name), buildEnv(root, name))
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if imagesFlags.quiet {
			seen := map[string]bool{}
			for _, key := range slices.Sorted(maps.Keys(p.Services)) {
				if img := p.Services[key].Image; img != "" && !seen[img] {
					seen[img] = true
					_, _ = fmt.Fprintln(out, img)
				}
			}
			return nil
		}
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "CONTAINER\tIMAGE")
		for _, key := range slices.Sorted(maps.Keys(p.Services)) {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", key, p.Services[key].Image)
		}
		return w.Flush()
	},
}

func init() {
	imagesCmd.Flags().BoolVarP(&imagesFlags.quiet, "quiet", "q", false, "Only display image references")
}
