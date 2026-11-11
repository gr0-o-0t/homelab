package bootstrap

// Replacing an agent container with one on a new image.
//
// An agent container cannot replace itself: the process doing it dies with
// the container it stops. So the agent starts a helper container from the
// new image with the docker socket, handing it the spec to recreate. The
// helper waits a moment (the agent finishes answering), stops the old agent
// and renames it aside (homelab-agent-old) — intact, same ID — then creates
// and starts the new one under the agent's name and watches it: only once
// it has run without restarting for a settle window is the old container
// removed. Anything else puts the old container back under its name and
// starts it again. No volume is ever removed or created.
//
// The helper removes itself when it succeeds. When it fails it stays
// (exited, no restart policy, labelled) so `docker logs homelab-agent-replace`
// says why; the next upgrade removes it first.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"

	"github.com/groot/homelab/internal/swarm"
)

const (
	// HelperContainer is the container that replaces an agent.
	HelperContainer = "homelab-agent-replace"
	// HelperLabel marks the helper, so a later run knows a leftover one is
	// ours to remove.
	HelperLabel = "homelab.agent-replace"
	// OldContainer is the old agent's name while its replacement proves
	// itself.
	OldContainer = AgentContainer + "-old"
	// ReplaceSpecEnv carries the helper its ReplaceSpec, as JSON. The spec
	// is the agent container's own, which holds no secret.
	ReplaceSpecEnv = "HOMELAB_REPLACE_SPEC"
	// VersionLabel is the image label holding the homelab version it runs.
	VersionLabel = "org.opencontainers.image.version"
	// ReplaceDelay is how long the helper waits before stopping the old
	// agent, so the command that started it can return its result.
	ReplaceDelay = 3 * time.Second
	// WaitBack bounds the wait for a replaced agent: the helper's for the
	// new container to be stable, upgrade's for it to answer.
	WaitBack = 60 * time.Second
	// Settle is how long the new agent must run without a restart before
	// the old one is removed.
	Settle = 10 * time.Second

	dockerSocketBind = "/var/run/docker.sock:/var/run/docker.sock"
	stopTimeout      = 30 // seconds the old agent gets to shut down
	pollEvery        = 2 * time.Second
	watchEvery       = time.Second
)

// ReplaceSpec is what the helper recreates: the old container's name, and
// its config, host config and networks as inspected, on the new image.
type ReplaceSpec struct {
	Container  string                   `json:"container"`
	Config     container.Config         `json:"config"`
	HostConfig container.HostConfig     `json:"host_config"`
	Networking network.NetworkingConfig `json:"networking"`
	Delay      time.Duration            `json:"delay"`
}

// Sleep waits d or until ctx is done (false).
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ReplacementSpec is the spec that recreates container name on img: its
// inspected config without what its old image set (env, labels, entrypoint,
// working dir, user, healthcheck — the new image's own apply), host config
// (every mount, the restart policy) and networks with their aliases.
func ReplacementSpec(ctx context.Context, api swarm.API, name, img string) (ReplaceSpec, error) {
	insp, err := api.ContainerInspect(ctx, name)
	if err != nil {
		return ReplaceSpec{}, fmt.Errorf("inspecting the %s container: %w", name, err)
	}
	if insp.ContainerJSONBase == nil || insp.Config == nil || insp.HostConfig == nil {
		return ReplaceSpec{}, fmt.Errorf("inspecting the %s container: incomplete answer from Docker", name)
	}
	cfg := *insp.Config
	if ii, _, err := api.ImageInspectWithRaw(ctx, insp.Image); err == nil && ii.Config != nil {
		dropImageDefaults(&cfg, ii.Config)
	}
	// The engine names an unnamed host after the container's short ID.
	if len(insp.ID) >= 12 && cfg.Hostname == insp.ID[:12] {
		cfg.Hostname = ""
	}
	cfg.Image = img
	spec := ReplaceSpec{Container: name, Config: cfg, HostConfig: *insp.HostConfig}
	if insp.NetworkSettings != nil && len(insp.NetworkSettings.Networks) > 0 {
		spec.Networking.EndpointsConfig = map[string]*network.EndpointSettings{}
		for net, ep := range insp.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			// Only what was asked for: addresses and IDs are the engine's.
			aliases := slices.DeleteFunc(slices.Clone(ep.Aliases), func(a string) bool {
				return a == insp.ID || (len(insp.ID) >= 12 && a == insp.ID[:12])
			})
			if len(aliases) == 0 {
				aliases = nil
			}
			spec.Networking.EndpointsConfig[net] = &network.EndpointSettings{
				Aliases: aliases, IPAMConfig: ep.IPAMConfig, Links: ep.Links, DriverOpts: ep.DriverOpts,
			}
		}
	}
	return spec, nil
}

// dropImageDefaults removes from cfg what equals its image's own config:
// the engine reports the two merged, and freezing the old image's values
// would override the new image's.
func dropImageDefaults(cfg *container.Config, img *dockerspec.DockerOCIImageConfig) {
	cfg.Env = slices.DeleteFunc(slices.Clone(cfg.Env), func(e string) bool { return slices.Contains(img.Env, e) })
	if len(cfg.Env) == 0 {
		cfg.Env = nil
	}
	if len(cfg.Labels) > 0 {
		labels := maps.Clone(cfg.Labels)
		maps.DeleteFunc(labels, func(k, v string) bool { iv, ok := img.Labels[k]; return ok && iv == v })
		cfg.Labels = labels
		if len(labels) == 0 {
			cfg.Labels = nil
		}
	}
	if len(cfg.Entrypoint) > 0 && slices.Equal([]string(cfg.Entrypoint), img.Entrypoint) {
		cfg.Entrypoint = nil
	}
	if cfg.WorkingDir == img.WorkingDir {
		cfg.WorkingDir = ""
	}
	if cfg.User == img.User {
		cfg.User = ""
	}
	if h, ih := cfg.Healthcheck, img.Healthcheck; h != nil && ih != nil && slices.Equal(h.Test, ih.Test) &&
		h.Interval == ih.Interval && h.Timeout == ih.Timeout && h.StartPeriod == ih.StartPeriod &&
		h.StartInterval == ih.StartInterval && h.Retries == ih.Retries {
		cfg.Healthcheck = nil
	}
}

// SelfReplace replaces this node's agent container with one on img: it
// makes img available, then starts the helper that does the replacement a
// few seconds later. It returns once the helper is running; the agent (and
// the command that called this) is stopped by the helper.
func SelfReplace(ctx context.Context, api swarm.API, img string, out io.Writer) error {
	if err := EnsureImage(ctx, api, img, out); err != nil {
		return err
	}
	if err := clearLeftovers(ctx, api, out); err != nil {
		return err
	}
	spec, err := ReplacementSpec(ctx, api, AgentContainer, img)
	if err != nil {
		return err
	}
	spec.Delay = ReplaceDelay
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	cfg := &container.Config{
		Image:  img,
		Cmd:    []string{"agent", "replace-helper"},
		Env:    []string{ReplaceSpecEnv + "=" + string(b)},
		Labels: map[string]string{HelperLabel: "1"},
	}
	// No AutoRemove and no restart policy: a failed helper stays, exited,
	// with its logs. A successful one removes itself.
	hc := &container.HostConfig{Binds: []string{dockerSocketBind}}
	if _, err := api.ContainerCreate(ctx, cfg, hc, nil, nil, HelperContainer); err != nil {
		return fmt.Errorf("creating the replacement helper: %w", err)
	}
	if err := api.ContainerStart(ctx, HelperContainer, container.StartOptions{}); err != nil {
		_ = api.ContainerRemove(context.WithoutCancel(ctx), HelperContainer, container.RemoveOptions{Force: true})
		return fmt.Errorf("starting the replacement helper: %w", err)
	}
	_, _ = fmt.Fprintf(out, "agent restarting: the %s container is replaced with %s in %s\n", AgentContainer, img, ReplaceDelay)
	return nil
}

// clearLeftovers removes a helper a failed earlier run left (exited and
// ours), and refuses while one runs, or while an old agent is still set
// aside (an interrupted replacement: a person should look first).
func clearLeftovers(ctx context.Context, api swarm.API, out io.Writer) error {
	if _, err := api.ContainerInspect(ctx, OldContainer); err == nil {
		return fmt.Errorf("a %s container exists: an earlier replacement was interrupted — check that %s runs, then `docker rm %s` and retry",
			OldContainer, AgentContainer, OldContainer)
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("checking for %s: %w", OldContainer, err)
	}
	h, err := api.ContainerInspect(ctx, HelperContainer)
	switch {
	case errdefs.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("checking for %s: %w", HelperContainer, err)
	case h.Config == nil || h.Config.Labels[HelperLabel] == "":
		return fmt.Errorf("a %s container exists that homelab did not create — remove it and retry", HelperContainer)
	case h.State != nil && h.State.Running:
		return fmt.Errorf("another agent replacement is in progress (%s is running)", HelperContainer)
	}
	_, _ = fmt.Fprintf(out, "Removing the helper a failed earlier replacement left (%s)\n", HelperContainer)
	if err := api.ContainerRemove(ctx, HelperContainer, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("removing the old %s: %w", HelperContainer, err)
	}
	return nil
}

// RunReplace is the helper's work (`homelab agent replace-helper`). After
// spec.Delay it stops the old agent and renames it aside, creates and starts
// the new one, and waits up to WaitBack for it to run Settle without a
// restart; then it removes the old container. On any failure the new
// container is removed and the old one renamed back and started again.
// sleep waits between checks (Sleep in production).
func RunReplace(ctx context.Context, api swarm.API, spec ReplaceSpec, sleep func(context.Context, time.Duration) bool) error {
	if spec.Container == "" || spec.Config.Image == "" {
		return errors.New("replace: incomplete spec")
	}
	if spec.Delay > 0 && !sleep(ctx, spec.Delay) {
		return ctx.Err()
	}
	old, err := api.ContainerInspect(ctx, spec.Container)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", spec.Container, err)
	}
	if _, err := api.ContainerInspect(ctx, OldContainer); err == nil {
		return fmt.Errorf("%s already exists: not touching %s", OldContainer, spec.Container)
	}
	oldID := old.ID
	t := stopTimeout
	if err := api.ContainerStop(ctx, oldID, container.StopOptions{Timeout: &t}); err != nil {
		return fmt.Errorf("stopping %s: %w", spec.Container, err)
	}
	r := &rollback{api: api, name: spec.Container, oldID: oldID}
	if err := api.ContainerRename(ctx, oldID, OldContainer); err != nil {
		return r.undo(ctx, fmt.Errorf("setting the old agent aside: %w", err))
	}
	r.renamed = true
	created, err := api.ContainerCreate(ctx, &spec.Config, &spec.HostConfig, &spec.Networking, nil, spec.Container)
	if err != nil {
		return r.undo(ctx, fmt.Errorf("creating the new agent: %w", err))
	}
	r.newID = created.ID
	if err := api.ContainerStart(ctx, r.newID, container.StartOptions{}); err != nil {
		return r.undo(ctx, fmt.Errorf("starting the new agent: %w", err))
	}
	if err := waitStable(ctx, api, r.newID, sleep); err != nil {
		return r.undo(ctx, err)
	}
	// Never RemoveVolumes: the agent's state, shim and backups volumes stay.
	if err := api.ContainerRemove(ctx, oldID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("the new agent runs, but removing %s failed (remove it by hand): %w", OldContainer, err)
	}
	// Done: the helper removes its own container. The engine kills this
	// process as part of that, so nothing may follow.
	_ = api.ContainerRemove(ctx, HelperContainer, container.RemoveOptions{Force: true})
	return nil
}

// waitStable waits for container id to run, without restarting, for Settle,
// giving up after WaitBack.
func waitStable(ctx context.Context, api swarm.API, id string, sleep func(context.Context, time.Duration) bool) error {
	steady := time.Duration(0)
	for waited := time.Duration(0); waited < WaitBack; waited += watchEvery {
		insp, err := api.ContainerInspect(ctx, id)
		if err != nil {
			return fmt.Errorf("watching the new agent: %w", err)
		}
		st := insp.State
		switch {
		case insp.RestartCount > 0:
			return fmt.Errorf("the new agent restarted %d time(s) — it crashes", insp.RestartCount)
		case st == nil:
			return errors.New("watching the new agent: no state from Docker")
		case st.Status == container.StateExited || st.Status == container.StateDead:
			return fmt.Errorf("the new agent exited (code %d)", st.ExitCode)
		case st.Running:
			steady += watchEvery
			if steady >= Settle {
				return nil
			}
		default:
			steady = 0
		}
		if !sleep(ctx, watchEvery) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("the new agent did not run steadily for %s within %s", Settle, WaitBack)
}

// rollback puts the old agent back: the new container removed, the old one
// renamed back and started, and checked to run.
type rollback struct {
	api          swarm.API
	name         string
	oldID, newID string
	renamed      bool
}

func (r *rollback) undo(ctx context.Context, cause error) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	if r.newID != "" {
		if err := r.api.ContainerRemove(ctx, r.newID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing the new agent: %w", err))
		}
	}
	if r.renamed {
		if err := r.api.ContainerRename(ctx, r.oldID, r.name); err != nil {
			errs = append(errs, fmt.Errorf("renaming %s back to %s: %w", OldContainer, r.name, err))
		}
	}
	if err := r.api.ContainerStart(ctx, r.oldID, container.StartOptions{}); err != nil {
		errs = append(errs, fmt.Errorf("starting the old agent again: %w", err))
	} else if insp, err := r.api.ContainerInspect(ctx, r.oldID); err != nil || insp.State == nil || !insp.State.Running {
		errs = append(errs, errors.New("the old agent does not run again"))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w; ROLLBACK FAILED: %w", cause, errors.Join(errs...))
	}
	return fmt.Errorf("%w (the old agent was put back and runs)", cause)
}
