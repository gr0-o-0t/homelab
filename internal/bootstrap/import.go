package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/importer"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
)

// builtImport is init --import's state tree, built on the host in a private
// temp dir, and the secrets read for it (in memory only).
type builtImport struct {
	dir     string
	report  importer.Report
	secrets importer.Secrets
}

// buildImport builds the tree for oldDir under a fresh 0700 temp dir in base
// ("" = the system's). The dir is removed again on error; on success the
// caller removes it (remove).
func buildImport(oldDir, base string, kr *secrets.Manager) (*builtImport, error) {
	dir, err := os.MkdirTemp(base, "homelab-import-*")
	if err != nil {
		return nil, fmt.Errorf("creating the import's temp dir: %w", err)
	}
	rep, sec, err := importer.Build(oldDir, dir, kr)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("importing %s: %w", oldDir, err)
	}
	return &builtImport{dir: dir, report: rep, secrets: sec}, nil
}

func (b *builtImport) remove() { _ = os.RemoveAll(b.dir) }

// install copies the tree into the agent container (a tar stream, no
// secret in it) and hands the secrets to `agent-seed --import` on stdin,
// which validates them against the copied config files, stores them and
// moves the tree into the state dir. The caller restarts the agent.
func (b *builtImport) install(ctx context.Context, api swarm.API) error {
	staging, err := importer.StagingName(nil)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := importer.Tar(&buf, b.dir, staging); err != nil {
		return fmt.Errorf("packing the imported state: %w", err)
	}
	if err := api.CopyToContainer(ctx, AgentContainer, agentstate.Root, &buf, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copying the imported state into %s: %w", AgentContainer, err)
	}
	cmd := []string{"homelab", "agent-seed", "--root", agentstate.Root, "--import", staging}
	if err := seedExec(ctx, api, cmd, b.secrets); err != nil {
		return fmt.Errorf("importing the state and secrets: %w", err)
	}
	return nil
}

// composeBridge returns the ID of the Compose-era home-services network (a
// local bridge, not the cluster's overlay), "" when there is none. It refuses
// one that still has containers attached: removing it would cut them off,
// and they are the Compose stack the import replaces.
func composeBridge(ctx context.Context, api swarm.API) (string, error) {
	list, err := api.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", swarm.SharedNetwork))})
	if err != nil {
		return "", fmt.Errorf("listing networks: %w", err)
	}
	for i := range list {
		n := &list[i]
		if n.Name != swarm.SharedNetwork || n.Driver == swarm.OverlayDriver {
			continue
		}
		ins, err := api.NetworkInspect(ctx, n.ID, network.InspectOptions{})
		if err != nil {
			return "", fmt.Errorf("inspecting network %s: %w", swarm.SharedNetwork, err)
		}
		if len(ins.Containers) > 0 {
			var names []string
			for id, c := range ins.Containers {
				if c.Name != "" {
					id = c.Name
				}
				names = append(names, id)
			}
			slices.Sort(names)
			return "", fmt.Errorf("the Compose-era network %s still has containers attached (%s) — "+
				"run `homelab down -a` and stop the core first, then retry",
				swarm.SharedNetwork, strings.Join(names, ", "))
		}
		return n.ID, nil
	}
	return "", nil
}

// removeComposeBridge removes an idle Compose-era home-services network, so
// the overlay of that name can be created.
func removeComposeBridge(ctx context.Context, api swarm.API) error {
	id, err := composeBridge(ctx, api)
	if err != nil || id == "" {
		return err
	}
	if err := api.NetworkRemove(ctx, id); err != nil {
		return fmt.Errorf("removing the Compose-era network %s: %w", swarm.SharedNetwork, err)
	}
	return nil
}

// printImportReport says what the import brought, changed and could not.
func printImportReport(w io.Writer, rep importer.Report) {
	_, _ = fmt.Fprintf(w, "\nImported %d services: %s.\n", len(rep.Services), strings.Join(rep.Services, ", "))
	if len(rep.Changed) > 0 {
		_, _ = fmt.Fprintln(w, "Changed on the way:")
		for _, c := range rep.Changed {
			_, _ = fmt.Fprintln(w, "  - "+c)
		}
	}
	if len(rep.MissingSecrets) > 0 {
		_, _ = fmt.Fprintln(w, "Secrets this machine's keyring did not have (set them, then `homelab up <service>`):")
		for _, k := range rep.MissingSecrets {
			setup := "homelab setup"
			if rest, ok := strings.CutPrefix(k, "svc:"); ok {
				svc, _, _ := strings.Cut(rest, ":")
				setup += " " + svc
			}
			_, _ = fmt.Fprintf(w, "  - %s: `%s`\n", k, setup)
		}
	}
}
