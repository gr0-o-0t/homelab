package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"text/tabwriter"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/swarm"
)

var imagesFlags struct {
	quiet bool
	all   bool
	group string
}

// imageInfo is one container's image in `images --json`: what a PC needs to
// decide whether to ship a locally built image to the leader.
type imageInfo struct {
	Service   string `json:"service"`
	Container string `json:"container"`
	Ref       string `json:"ref"`
	Local     bool   `json:"local"`
	// Identity is the image's identity on this engine (swarm.ImageIdentity),
	// "" when the engine does not have it (or the image is not local).
	Identity string `json:"identity"`
}

var imagesCmd = &cobra.Command{
	Use:   "images <service>",
	Short: "List the Docker images a service deploys",
	Long: `List the image of every container of a service's stack, as its compose file
resolves them.

  homelab images jellyfin     # compose key and image
  homelab images -q immich    # image references only
  homelab images --json -a    # several services, with local images' identities on the leader`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()
		if rootFlags.json {
			names, err := resolveTargets(root, "images", imagesFlags.all, imagesFlags.group, args)
			if err != nil {
				return err
			}
			return imagesJSON(cmd.OutOrStdout(), root, names)
		}
		if len(args) != 1 || imagesFlags.all || imagesFlags.group != "" {
			return fmt.Errorf("name exactly one service (several, --all and --group need --json)")
		}
		name := args[0]
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

// imagesJSON writes every container's image for the named services. A local
// image (compose pull_policy: never, the translator's rule) carries the
// identity this engine has for it.
func imagesJSON(out io.Writer, root string, names []string) error {
	infos := []imageInfo{}
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
		p, err := swarm.LoadProject(filepath.Join(root, "services", name), buildEnv(root, name))
		if err != nil {
			return err
		}
		for _, key := range slices.Sorted(maps.Keys(p.Services)) {
			cs := p.Services[key]
			infos = append(infos, imageInfo{
				Service: name, Container: key, Ref: cs.Image,
				Local: cs.PullPolicy == types.PullPolicyNever,
			})
		}
	}
	if slices.ContainsFunc(infos, func(i imageInfo) bool { return i.Local }) {
		err := withDeployer(root, false, func(ctx context.Context, d *swarm.Deployer) error {
			for i := range infos {
				if !infos[i].Local {
					continue
				}
				id, err := engineIdentity(ctx, d.API, infos[i].Ref)
				if err != nil {
					return err
				}
				infos[i].Identity = id
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(infos)
}

// engineIdentity is ref's identity on the engine, "" when it has no such image.
func engineIdentity(ctx context.Context, api swarm.API, ref string) (string, error) {
	img, _, err := api.ImageInspectWithRaw(ctx, ref)
	switch {
	case errdefs.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("inspecting image %s: %w", ref, err)
	}
	return swarm.ImageIdentity(img), nil
}

// imagesLoadCmd is plumbing for a PC shipping a locally built image: the
// `docker save` tar on stdin is loaded into the leader's engine, and each ref
// named is reported with the identity it now has there.
var imagesLoadCmd = &cobra.Command{
	Use:    "load <ref>...",
	Short:  "Load a docker save tar from stdin into the engine and report the refs' identities",
	Hidden: true,
	Args:   cobra.MinimumNArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		return withDeployer(configDir(), false, func(ctx context.Context, d *swarm.Deployer) error {
			res, err := d.API.ImageLoad(ctx, c.InOrStdin())
			if err != nil {
				return fmt.Errorf("loading image: %w", err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			if err := res.Body.Close(); err != nil {
				return err
			}
			enc := json.NewEncoder(c.OutOrStdout())
			for _, ref := range args {
				id, err := engineIdentity(ctx, d.API, ref)
				if err != nil {
					return err
				}
				if id == "" {
					return fmt.Errorf("%w: image %s is not on the engine after the load", errImageNotLoaded, ref)
				}
				if err := enc.Encode(map[string]string{"ref": ref, "identity": id}); err != nil {
					return err
				}
			}
			return nil
		})
	},
}

var errImageNotLoaded = errors.New("image not loaded")

func init() {
	imagesCmd.Flags().BoolVarP(&imagesFlags.quiet, "quiet", "q", false, "Only display image references")
	imagesCmd.Flags().BoolVarP(&imagesFlags.all, "all", "a", false, "With --json: every installed service")
	imagesCmd.Flags().StringVar(&imagesFlags.group, "group", "", "With --json: a named service group")
	_ = imagesCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
	imagesCmd.AddCommand(imagesLoadCmd)
}
