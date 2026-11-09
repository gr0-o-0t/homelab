package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

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
	// Streamed: a service may bind-mount a large directory.
	pr, pw := io.Pipe()
	tarDone := make(chan error, 1)
	go func() {
		err := importer.Tar(pw, b.dir, staging)
		_ = pw.CloseWithError(err)
		tarDone <- err
	}()
	err = api.CopyToContainer(ctx, AgentContainer, agentstate.Root, pr, container.CopyToContainerOptions{})
	_ = pr.CloseWithError(io.ErrClosedPipe) // unblocks the writer if the copy stopped reading
	terr := <-tarDone
	if err != nil {
		return fmt.Errorf("copying the imported state into %s: %w", AgentContainer, err)
	}
	// The copy succeeded, but a writer error — even the closed pipe of a
	// copy that stopped reading early — means the container got a truncated
	// tar, which must never reach agent-seed.
	if terr != nil {
		return fmt.Errorf("packing the imported state (the copy into %s is incomplete): %w", AgentContainer, terr)
	}
	cmd := []string{"homelab", "agent-seed", "--root", agentstate.Root, "--import", staging}
	if err := seedExec(ctx, api, cmd, b.secrets); err != nil {
		return fmt.Errorf("importing the state and secrets: %w", err)
	}
	return nil
}

// composeWorkingDirLabel is the label compose puts on every container of a
// project: the project's directory.
const composeWorkingDirLabel = "com.docker.compose.project.working_dir"

// composeContainers refuses any container, running or stopped, of a Compose
// project under oldDir (the core, a service): the import replaces that
// stack, and a stopped one would come back on the next `docker start` or
// boot beside its Swarm twin. Other projects' containers are not ours.
func composeContainers(ctx context.Context, api swarm.API, oldDir string) error {
	root, err := filepath.Abs(oldDir)
	if err != nil {
		return err
	}
	// Compose recorded whichever spelling it was run with: a symlinked
	// ~/.config matches either way.
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		real = root
	}
	list, err := api.ContainerList(ctx, container.ListOptions{All: true,
		Filters: filters.NewArgs(filters.Arg("label", composeWorkingDirLabel))})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	var names []string
	for i := range list {
		c := &list[i]
		wd := c.Labels[composeWorkingDirLabel]
		// Either side may be the symlinked spelling.
		wdReal, rerr := filepath.EvalSymlinks(wd)
		if rerr != nil {
			wdReal = wd
		}
		under := within(root, wd) || within(real, wd) || within(root, wdReal) || within(real, wdReal)
		if wd == "" || !under {
			continue
		}
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil
	}
	slices.Sort(names)
	return fmt.Errorf("the Compose-era stack under %s still has containers (%s) — run `homelab down -a` "+
		"and stop the core first (`docker compose -f %s down`), then retry",
		root, strings.Join(names, ", "), filepath.Join(root, "core", "docker-compose.yml"))
}

// within reports whether path is dir or lies under it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// stateVolumeExists reports whether the agent's state volume exists.
func stateVolumeExists(ctx context.Context, api swarm.API) (bool, error) {
	res, err := api.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs(filters.Arg("name", StateVolume))})
	if err != nil {
		return false, fmt.Errorf("listing volumes: %w", err)
	}
	for _, v := range res.Volumes {
		if v != nil && v.Name == StateVolume {
			return true, nil
		}
	}
	return false, nil
}

// refuseLeftoverStateVolume refuses an import onto a state volume an earlier
// attempt left: Docker would silently reuse it (vault included), and the
// rollback must never delete something this run did not create.
func refuseLeftoverStateVolume(ctx context.Context, api swarm.API) error {
	exists, err := stateVolumeExists(ctx, api)
	if err != nil || !exists {
		return err
	}
	return fmt.Errorf("a %s volume from an earlier attempt exists; if it holds nothing you need, remove it with "+
		"`docker volume rm %s` (data volumes are untouched)", StateVolume, StateVolume)
}

// removeAgentAndState undoes a failed import: the agent container goes and,
// when this run created it (ownVolume), its state volume (StateVolume), so a
// retry starts clean. No other volume is ever removed — the services' data
// volumes stay as they are.
func removeAgentAndState(ctx context.Context, api swarm.API, out io.Writer, ownVolume bool) {
	cerr := api.ContainerRemove(ctx, AgentContainer, container.RemoveOptions{Force: true})
	verr := error(nil)
	if cerr == nil && !ownVolume {
		_, _ = fmt.Fprintf(out, "Import failed: removed the agent; its state volume %s predates this run and is kept.\n",
			StateVolume)
		return
	}
	if cerr == nil {
		verr = api.VolumeRemove(ctx, StateVolume, true)
	}
	if cerr != nil || verr != nil {
		_, _ = fmt.Fprintf(out, "warning: could not undo the import (%v) — run `docker rm -f %s && docker volume rm %s` "+
			"before retrying; data volumes are untouched\n", errors.Join(cerr, verr), AgentContainer, StateVolume)
		return
	}
	_, _ = fmt.Fprintf(out, "Import failed: removed the agent and its state volume (%s) so a retry starts clean; "+
		"data volumes are untouched.\n", StateVolume)
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
