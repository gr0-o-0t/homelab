package bootstrap

// `homelab upgrade`: roll a new agent image across the cluster. The leader
// pulls the image on every node (each worker through its own agent), then
// replaces the worker agents one by one, each back on the new version before
// the next, and its own agent last.
//
// An agent container cannot replace itself: the process doing it dies with
// the container it removes. So the agent starts a short-lived helper
// container from the new image with the docker socket, handing it the spec
// to recreate; the helper waits a moment (the agent finishes answering),
// stops and removes the old container, creates and starts the new one with
// the identical spec on the new image, and exits (AutoRemove). No volume is
// ever removed or created: the new container mounts the same ones.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/swarm"
)

const (
	// HelperContainer is the short-lived container that replaces an agent.
	HelperContainer = "homelab-agent-replace"
	// ReplaceSpecEnv carries the helper its ReplaceSpec, as JSON. The spec
	// is the agent container's own, which holds no secret.
	ReplaceSpecEnv = "HOMELAB_REPLACE_SPEC"
	// VersionLabel is the image label holding the homelab version it runs.
	VersionLabel = "org.opencontainers.image.version"
	// ReplaceDelay is how long the helper waits before stopping the old
	// agent, so the command that started it can return its result.
	ReplaceDelay = 3 * time.Second
	// WaitBack is how long upgrade waits for a replaced agent to answer.
	WaitBack = 60 * time.Second

	dockerSocketBind = "/var/run/docker.sock:/var/run/docker.sock"
	stopTimeout      = 30 // seconds the old agent gets to shut down
	pollEvery        = 2 * time.Second
)

// ReplaceSpec is what the helper recreates: the old container's name, and
// its config, host config and networks as inspected, on the new image.
type ReplaceSpec struct {
	Container string `json:"container"`
	// OldImage is the old container's image ID, to roll back to when the
	// new one does not start (a tag may already point at the new image).
	OldImage   string                   `json:"old_image"`
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
// inspected config (env and labels without the ones its old image set, so
// the new image's own apply), host config (every mount, the restart policy)
// and networks with their aliases.
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
		cfg.Env = slices.DeleteFunc(slices.Clone(cfg.Env), func(e string) bool { return slices.Contains(ii.Config.Env, e) })
		if len(cfg.Labels) > 0 {
			labels := maps.Clone(cfg.Labels)
			maps.DeleteFunc(labels, func(k, v string) bool { iv, ok := ii.Config.Labels[k]; return ok && iv == v })
			cfg.Labels = labels
			if len(labels) == 0 {
				cfg.Labels = nil
			}
		}
	}
	// The engine names an unnamed host after the container's short ID.
	if len(insp.ID) >= 12 && cfg.Hostname == insp.ID[:12] {
		cfg.Hostname = ""
	}
	cfg.Image = img
	spec := ReplaceSpec{Container: name, OldImage: insp.Image, Config: cfg, HostConfig: *insp.HostConfig}
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

// SelfReplace replaces this node's agent container with one on img: it
// makes img available, then starts the helper that does the replacement a
// few seconds later. It returns once the helper is running; the agent (and
// the command that called this) is stopped by the helper.
func SelfReplace(ctx context.Context, api swarm.API, img string, out io.Writer) error {
	if err := EnsureImage(ctx, api, img, out); err != nil {
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
		Image: img,
		Cmd:   []string{"agent", "replace-helper"},
		Env:   []string{ReplaceSpecEnv + "=" + string(b)},
	}
	hc := &container.HostConfig{Binds: []string{dockerSocketBind}, AutoRemove: true}
	if _, err := api.ContainerCreate(ctx, cfg, hc, nil, nil, HelperContainer); err != nil {
		if errdefs.IsConflict(err) {
			return fmt.Errorf("another agent replacement is in progress (container %s exists) — if it is stuck, `docker rm -f %s` and retry: %w",
				HelperContainer, HelperContainer, err)
		}
		return fmt.Errorf("creating the replacement helper: %w", err)
	}
	if err := api.ContainerStart(ctx, HelperContainer, container.StartOptions{}); err != nil {
		_ = api.ContainerRemove(context.WithoutCancel(ctx), HelperContainer, container.RemoveOptions{Force: true})
		return fmt.Errorf("starting the replacement helper: %w", err)
	}
	_, _ = fmt.Fprintf(out, "agent restarting: the %s container is replaced with %s in %s\n", AgentContainer, img, ReplaceDelay)
	return nil
}

// RunReplace is the helper's work (`homelab agent replace-helper`): after
// spec.Delay, stop and remove the old container (its volumes kept), create
// and start the new one. When the new one does not start, the old image is
// put back with the same spec.
func RunReplace(ctx context.Context, api swarm.API, spec ReplaceSpec, sleep func(context.Context, time.Duration) bool) error {
	if spec.Container == "" || spec.Config.Image == "" {
		return errors.New("replace: incomplete spec")
	}
	if spec.Delay > 0 && !sleep(ctx, spec.Delay) {
		return ctx.Err()
	}
	t := stopTimeout
	if err := api.ContainerStop(ctx, spec.Container, container.StopOptions{Timeout: &t}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stopping %s: %w", spec.Container, err)
	}
	// Never RemoveVolumes: the agent's state, shim and backups volumes stay.
	if err := api.ContainerRemove(ctx, spec.Container, container.RemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		_ = api.ContainerStart(ctx, spec.Container, container.StartOptions{})
		return fmt.Errorf("removing %s: %w", spec.Container, err)
	}
	err := createAndStart(ctx, api, spec.Container, spec.Config, spec.HostConfig, spec.Networking)
	if err == nil {
		return nil
	}
	if spec.OldImage == "" {
		return fmt.Errorf("starting the new %s: %w", spec.Container, err)
	}
	_ = api.ContainerRemove(ctx, spec.Container, container.RemoveOptions{Force: true})
	old := spec.Config
	old.Image = spec.OldImage
	if rerr := createAndStart(ctx, api, spec.Container, old, spec.HostConfig, spec.Networking); rerr != nil {
		return fmt.Errorf("starting the new %s: %w; rolling back to the old image also failed: %w", spec.Container, err, rerr)
	}
	return fmt.Errorf("starting the new %s: %w (rolled back to the old image)", spec.Container, err)
}

func createAndStart(ctx context.Context, api swarm.API, name string, cfg container.Config, hc container.HostConfig, nc network.NetworkingConfig) error {
	if _, err := api.ContainerCreate(ctx, &cfg, &hc, &nc, nil, name); err != nil {
		return err
	}
	return api.ContainerStart(ctx, name, container.StartOptions{})
}

// WorkerStatus is how upgrade left one worker.
type WorkerStatus string

const (
	// WorkerUpgraded: replaced, and back on the new version.
	WorkerUpgraded WorkerStatus = "upgraded"
	// WorkerCurrent: already on the new version; not touched.
	WorkerCurrent WorkerStatus = "already current"
	// WorkerSkipped: offline (node down, or its agent unreachable).
	WorkerSkipped WorkerStatus = "skipped"
	// WorkerFailed: its pull or replacement failed, or it came back on
	// another version. The rollout halts there.
	WorkerFailed WorkerStatus = "failed"
	// WorkerNotReached: the rollout halted before it.
	WorkerNotReached WorkerStatus = "not reached"
)

// WorkerResult is one worker's outcome.
type WorkerResult struct {
	Host    string
	Status  WorkerStatus
	Version string // what its agent reports, when known
	Detail  string
}

// AgentCall runs argv on the agent of worker host and returns its stdout.
// An agent that cannot be reached is an error matching remote.ErrUnreachable.
type AgentCall func(ctx context.Context, host string, argv []string) (string, error)

// Upgrade is `homelab upgrade` on the leader.
type Upgrade struct {
	API   swarm.API // the leader's engine
	Image string
	// Version is what the upgraded agents must report; "" reads the image's
	// VersionLabel, and without one versions are reported, not checked.
	Version string
	Call    AgentCall
	// Replace replaces the leader's own agent; default SelfReplace.
	Replace func(ctx context.Context) error
	Out     io.Writer
	// Sleep waits between polls; default Sleep.
	Sleep func(ctx context.Context, d time.Duration) bool
}

func (u *Upgrade) sleep(ctx context.Context, d time.Duration) bool {
	if u.Sleep != nil {
		return u.Sleep(ctx, d)
	}
	return Sleep(ctx, d)
}

func (u *Upgrade) printf(format string, args ...any) { _, _ = fmt.Fprintf(u.Out, format, args...) }

// Do runs the rollout. It returns every worker's result; an error means it
// halted before replacing the leader.
func (u *Upgrade) Do(ctx context.Context) ([]WorkerResult, error) {
	if u.Image == "" {
		return nil, errors.New("no agent image")
	}
	u.printf("Upgrading the cluster's agents to %s\n", u.Image)
	if err := EnsureImage(ctx, u.API, u.Image, u.Out); err != nil {
		return nil, fmt.Errorf("on the leader: %w", err)
	}
	want := u.Version
	if want == "" {
		if ii, _, err := u.API.ImageInspectWithRaw(ctx, u.Image); err == nil && ii.Config != nil {
			want = ii.Config.Labels[VersionLabel]
		}
	}
	if want == "" {
		u.printf("warning: %s carries no %s label: agents' versions are reported, not checked\n", u.Image, VersionLabel)
	}
	nodes, err := u.API.NodeList(ctx, dswarm.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the swarm's nodes: %w", err)
	}
	workers := slices.DeleteFunc(slices.Clone(nodes), func(n dswarm.Node) bool { return n.Spec.Role != dswarm.NodeRoleWorker })
	sort.SliceStable(workers, func(a, b int) bool { return workers[a].Description.Hostname < workers[b].Description.Hostname })
	res := make([]WorkerResult, len(workers))
	for i := range workers {
		res[i].Host = workers[i].Description.Hostname
	}

	// 1. Every worker answers and has the image before any agent changes.
	pull := []string{"agent", "self-replace", "--image", u.Image, "--pull-only"}
	halted := false
	for i := range res {
		r := &res[i]
		if workers[i].Status.State != dswarm.NodeStateReady {
			r.Status, r.Detail = WorkerSkipped, "offline (swarm node not ready)"
			continue
		}
		v, err := u.version(ctx, r.Host)
		if err == nil && want != "" && v == want {
			r.Status, r.Version = WorkerCurrent, v
			continue
		}
		if err == nil {
			r.Version = v
			_, err = u.Call(ctx, r.Host, pull)
		}
		switch {
		case errors.Is(err, remote.ErrUnreachable):
			r.Status, r.Detail = WorkerSkipped, "offline (agent unreachable)"
		case err != nil:
			r.Status, r.Detail, halted = WorkerFailed, "pulling the image: "+err.Error(), true
		}
	}
	if halted {
		u.notReached(res)
		u.summary(res)
		return res, errors.New("the image is not available on every reachable worker; no agent was replaced — fix the failure above and rerun `homelab upgrade`")
	}

	// 2. Workers one by one, each back before the next.
	for i := range res {
		r := &res[i]
		if r.Status != "" {
			continue
		}
		u.printf("Replacing the agent on %s …\n", r.Host)
		u.replaceWorker(ctx, r, want)
		if r.Status == WorkerFailed {
			u.notReached(res)
			u.summary(res)
			return res, fmt.Errorf("the agent on %s: %s — halted before the leader; rerun `homelab upgrade` once it is fixed", r.Host, r.Detail)
		}
	}
	u.summary(res)

	// 3. The leader last: this ends the session.
	u.printf("Replacing the leader agent — agent restarting; this session ends here.\n")
	replace := u.Replace
	if replace == nil {
		replace = func(ctx context.Context) error { return SelfReplace(ctx, u.API, u.Image, u.Out) }
	}
	if err := replace(ctx); err != nil {
		return res, fmt.Errorf("replacing the leader agent: %w", err)
	}
	return res, nil
}

// version is the version host's agent reports.
func (u *Upgrade) version(ctx context.Context, host string) (string, error) {
	out, err := u.Call(ctx, host, []string{"version"})
	return strings.TrimSpace(out), err
}

// replaceWorker replaces host's agent and waits for it to answer on want.
func (u *Upgrade) replaceWorker(ctx context.Context, r *WorkerResult, want string) {
	_, err := u.Call(ctx, r.Host, []string{"agent", "self-replace", "--image", u.Image})
	switch {
	case errors.Is(err, remote.ErrUnreachable):
		r.Status, r.Detail = WorkerSkipped, "went offline before its agent was replaced"
		return
	case err != nil:
		r.Status, r.Detail = WorkerFailed, "replacing: "+err.Error()
		return
	}
	// The old agent answers until the helper stops it.
	if !u.sleep(ctx, ReplaceDelay) {
		r.Status, r.Detail = WorkerFailed, ctx.Err().Error()
		return
	}
	last := ""
	for range int(WaitBack / pollEvery) {
		v, err := u.version(ctx, r.Host)
		if err == nil {
			last = v
			if want == "" || v == want {
				r.Status, r.Version = WorkerUpgraded, v
				return
			}
		}
		if !u.sleep(ctx, pollEvery) {
			break
		}
	}
	if last == "" {
		r.Status, r.Detail = WorkerFailed, fmt.Sprintf("did not answer within %s of being replaced — see `docker logs %s` there", WaitBack, AgentContainer)
		return
	}
	r.Status, r.Version = WorkerFailed, last
	r.Detail = fmt.Sprintf("version mismatch: reports %s, expected %s", last, want)
}

func (u *Upgrade) notReached(res []WorkerResult) {
	for i := range res {
		if res[i].Status == "" {
			res[i].Status = WorkerNotReached
		}
	}
}

func (u *Upgrade) summary(res []WorkerResult) {
	if len(res) == 0 {
		u.printf("No worker nodes.\n")
		return
	}
	u.printf("Workers:\n")
	for _, r := range res {
		line := fmt.Sprintf("  %-20s %s", r.Host, r.Status)
		if r.Version != "" {
			line += " (" + r.Version + ")"
		}
		if r.Detail != "" {
			line += ": " + r.Detail
		}
		u.printf("%s\n", line)
	}
}
