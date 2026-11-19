package bootstrap

// The leader's agent runs as the Swarm service homelab_agent, not as a plain
// container: a plain container on the home-services overlay is started at
// engine boot through a network attachment the engine never retries, so one
// failed attach (Swarm's IPAM still holding its old address) left the
// cluster without its agent. Swarm schedules a service's task, network
// included, and retries it forever — as it does every other workload.
// Worker agents are not on the overlay and stay plain containers.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/swarm"
)

const (
	// AgentService is the leader agent's Swarm service.
	AgentService = "homelab_agent"
	// AgentServiceLabel is its homelab.service label and stack namespace:
	// a reserved service name, so no per-service command ever matches it.
	AgentServiceLabel = config.ReservedServiceName
	// LeaderWaitBack bounds a PC's wait for the leader agent to answer
	// again after upgrade replaced it. Longer than a worker's WaitBack: the
	// first upgrade onto the service waits out the old swap, converts, and
	// boots the edge cold.
	LeaderWaitBack = 3 * time.Minute
	// leaderMonitor is how long Swarm watches a new leader task before an
	// update counts as done; one that exits sooner is rolled back.
	leaderMonitor = 30 * time.Second
)

// LeaderServiceSpec is the leader agent as a Swarm service on img: the same
// command, env and volumes as AgentSpec(img, true), on home-services as
// homelab-edge, pinned to the manager. Updates and rollbacks are stop-first:
// two leader agents would bring up the same tailnet node from the same state.
func LeaderServiceSpec(img string) dswarm.ServiceSpec {
	cfg, hc, _ := AgentSpec(img, true)
	var mounts []mount.Mount
	for _, b := range hc.Binds {
		src, dst, _ := strings.Cut(b, ":")
		typ := mount.TypeVolume
		if strings.HasPrefix(src, "/") {
			typ = mount.TypeBind
		}
		mounts = append(mounts, mount.Mount{Type: typ, Source: src, Target: dst})
	}
	one := uint64(1)
	return dswarm.ServiceSpec{
		Annotations: dswarm.Annotations{
			Name: AgentService,
			Labels: map[string]string{
				swarm.LabelNamespace: AgentServiceLabel,
				swarm.LabelService:   AgentServiceLabel,
				swarm.LabelRole:      string(swarm.RoleCore),
			},
		},
		TaskTemplate: dswarm.TaskSpec{
			ContainerSpec: &dswarm.ContainerSpec{
				Image:  img,
				Args:   cfg.Cmd,
				Env:    cfg.Env,
				Mounts: mounts,
			},
			RestartPolicy: &dswarm.RestartPolicy{Condition: dswarm.RestartPolicyConditionAny},
			Networks:      []dswarm.NetworkAttachmentConfig{{Target: swarm.SharedNetwork, Aliases: []string{EdgeAlias}}},
			Placement: &dswarm.Placement{
				Constraints: append([]string{"node.role==manager"}, swarm.Constraints(swarm.RoleCore, false)...),
			},
		},
		Mode: dswarm.ServiceMode{Replicated: &dswarm.ReplicatedService{Replicas: &one}},
		UpdateConfig: &dswarm.UpdateConfig{
			Parallelism: 1, Order: dswarm.UpdateOrderStopFirst,
			FailureAction: dswarm.UpdateFailureActionRollback, Monitor: leaderMonitor,
		},
		RollbackConfig: &dswarm.UpdateConfig{
			Parallelism: 1, Order: dswarm.UpdateOrderStopFirst,
			FailureAction: dswarm.UpdateFailureActionPause, Monitor: leaderMonitor,
		},
	}
}

// AgentContainerID is this engine's agent container: the running task of
// the leader service, else the container named homelab-agent (a worker, or
// a leader not yet converted). A NotFound error when there is neither.
func AgentContainerID(ctx context.Context, api swarm.API) (string, error) {
	if id, err := leaderTaskContainer(ctx, api); err != nil || id != "" {
		return id, err
	}
	insp, err := api.ContainerInspect(ctx, AgentContainer)
	if err != nil {
		return "", err
	}
	if insp.ContainerJSONBase == nil {
		return "", fmt.Errorf("inspecting %s: incomplete answer from Docker", AgentContainer)
	}
	return insp.ID, nil
}

// leaderTaskContainer is the running task container of the leader service
// on this engine; "" when there is none.
func leaderTaskContainer(ctx context.Context, api swarm.API) (string, error) {
	list, err := api.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(
		filters.Arg("label", SwarmServiceLabel+"="+AgentService),
		filters.Arg("status", container.StateRunning),
	)})
	if err != nil {
		return "", fmt.Errorf("looking for the %s task: %w", AgentService, err)
	}
	if len(list) == 0 {
		return "", nil
	}
	return list[0].ID, nil
}

// StartLeaderService creates the leader service on img (init). The image
// is already on the engine; the registry is not queried, so a local image
// (homelab:dev) works as it does for the container.
func StartLeaderService(ctx context.Context, api swarm.API, img string) error {
	if _, err := api.ServiceCreate(ctx, LeaderServiceSpec(img), dswarm.ServiceCreateOptions{}); err != nil {
		return fmt.Errorf("creating the %s service: %w", AgentService, err)
	}
	return nil
}

// waitLeaderTask waits up to WaitBack for the leader service's task
// container to run on this engine and returns its ID.
func waitLeaderTask(ctx context.Context, api swarm.API, sleep func(context.Context, time.Duration) bool) (string, error) {
	for waited := time.Duration(0); ; waited += pollEvery {
		id, err := leaderTaskContainer(ctx, api)
		if err != nil || id != "" {
			return id, err
		}
		if waited >= WaitBack {
			return "", fmt.Errorf("the %s service has no running task after %s — see `docker service ps %s`", AgentService, WaitBack, AgentService)
		}
		if !sleep(ctx, pollEvery) {
			return "", ctx.Err()
		}
	}
}

// leaderService is the leader service, ok false when there is none.
func leaderService(ctx context.Context, api swarm.API) (dswarm.Service, bool, error) {
	s, _, err := api.ServiceInspectWithRaw(ctx, AgentService, dswarm.ServiceInspectOptions{})
	switch {
	case errdefs.IsNotFound(err):
		return dswarm.Service{}, false, nil
	case err != nil:
		return dswarm.Service{}, false, fmt.Errorf("inspecting the %s service: %w", AgentService, err)
	}
	return s, true, nil
}

// removeLeaderService removes the leader service and, at once, its task
// containers (Swarm would, a moment later), so the state volume they mount
// can go next. No service is not an error.
func removeLeaderService(ctx context.Context, api swarm.API) error {
	if _, ok, err := leaderService(ctx, api); err != nil || !ok {
		return err
	}
	if err := api.ServiceRemove(ctx, AgentService); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing the %s service: %w", AgentService, err)
	}
	return removeLeaderTasks(ctx, api)
}

// removeLeaderTasks force-removes every task container of the leader
// service on this engine.
func removeLeaderTasks(ctx context.Context, api swarm.API) error {
	list, err := api.ContainerList(ctx, container.ListOptions{All: true,
		Filters: filters.NewArgs(filters.Arg("label", SwarmServiceLabel+"="+AgentService))})
	if err != nil {
		return fmt.Errorf("listing the %s tasks: %w", AgentService, err)
	}
	for _, c := range list {
		if err := api.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("removing the %s task container: %w", AgentService, err)
		}
	}
	return nil
}

// StartConvert hands the leader's plain agent container to the helper,
// which makes it the leader service on img (RunConvert). It returns once
// the helper runs; the helper stops this agent a few seconds later.
func StartConvert(ctx context.Context, api swarm.API, img string, out io.Writer) error {
	if err := clearLeftovers(ctx, api, out); err != nil {
		return err
	}
	spec, err := ReplacementSpec(ctx, api, AgentContainer, img)
	if err != nil {
		return err
	}
	spec.Delay, spec.ToService = ReplaceDelay, true
	if err := startHelper(ctx, api, img, spec); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "agent restarting: the %s container becomes the %s service on %s in %s\n",
		AgentContainer, AgentService, img, ReplaceDelay)
	return nil
}

// RunConvert is the helper's work when spec.ToService: after spec.Delay it
// stops the plain agent container and sets it aside, then runs the leader
// service on spec.Config.Image — created, or an idle one reused — and waits
// for its task to run steadily; then it removes the old container. A
// service already running a task is left alone: the container only takes
// itself away. On failure the service is removed (or put back as it was)
// with its task before the old container is renamed back and started, so
// two leader agents never run at once.
func RunConvert(ctx context.Context, api swarm.API, spec ReplaceSpec, sleep func(context.Context, time.Duration) bool) error {
	info, err := api.Info(ctx)
	if err != nil {
		return fmt.Errorf("asking Docker for this node's swarm role: %w", err)
	}
	if !info.Swarm.ControlAvailable {
		return errors.New("not a swarm manager: only the leader's agent runs as the " + AgentService + " service")
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
	svc, exists, err := leaderService(ctx, api)
	if err != nil {
		return err
	}
	running, err := leaderTaskContainer(ctx, api)
	if err != nil {
		return err
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

	if !exists || running == "" {
		undoService := func() error { return removeLeaderService(context.WithoutCancel(ctx), api) }
		if exists {
			prev := svc.Spec
			undoService = func() error { return restoreLeaderService(context.WithoutCancel(ctx), api, prev) }
			next := LeaderServiceSpec(spec.Config.Image)
			next.TaskTemplate.ForceUpdate = prev.TaskTemplate.ForceUpdate + 1
			if _, err := api.ServiceUpdate(ctx, svc.ID, svc.Version, next, dswarm.ServiceUpdateOptions{}); err != nil {
				return r.undo(ctx, fmt.Errorf("updating the %s service: %w", AgentService, err))
			}
		} else if err := StartLeaderService(ctx, api, spec.Config.Image); err != nil {
			return r.undo(ctx, err)
		}
		id, err := waitLeaderTask(ctx, api, sleep)
		if err == nil {
			err = waitStable(ctx, api, id, sleep)
		}
		if err != nil {
			if serr := undoService(); serr != nil {
				err = fmt.Errorf("%w; and putting the %s service back: %w", err, AgentService, serr)
			}
			return r.undo(ctx, err)
		}
	}
	// Never RemoveVolumes: the agent's state, shim and backups volumes stay.
	if err := api.ContainerRemove(ctx, oldID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("the %s service runs, but removing %s failed (remove it by hand): %w", AgentService, OldContainer, err)
	}
	// Done: the helper removes its own container, which ends this process.
	_ = api.ContainerRemove(ctx, HelperContainer, container.RemoveOptions{Force: true})
	return nil
}

// restoreLeaderService puts the leader service back to spec and removes
// any task container of it at once.
func restoreLeaderService(ctx context.Context, api swarm.API, spec dswarm.ServiceSpec) error {
	s, ok, err := leaderService(ctx, api)
	if err != nil || !ok {
		return err
	}
	if _, err := api.ServiceUpdate(ctx, s.ID, s.Version, spec, dswarm.ServiceUpdateOptions{}); err != nil {
		return err
	}
	return removeLeaderTasks(ctx, api)
}

// PrepareLeader runs when the agent starts as the leader. self is its own
// container (its hostname). A task of the leader service: nothing to do. The
// plain homelab-agent container: it hands itself to the helper to become
// the service (true: the agent must not start the edge; the helper stops
// it). It first waits out a replacement still in progress — the helper of
// an older upgrade watches this very container and would roll back one
// that stops — and gives up after 2×WaitBack, returning an error: the agent
// then runs as a plain container for this boot.
func PrepareLeader(ctx context.Context, api swarm.API, self string, sleep func(context.Context, time.Duration) bool, out io.Writer) (bool, error) {
	insp, err := api.ContainerInspect(ctx, self)
	if err != nil {
		return false, fmt.Errorf("inspecting this agent's container: %w", err)
	}
	if insp.ContainerJSONBase == nil || insp.Config == nil {
		return false, errors.New("inspecting this agent's container: incomplete answer from Docker")
	}
	if insp.Config.Labels[SwarmServiceLabel] == AgentService || strings.TrimPrefix(insp.Name, "/") != AgentContainer {
		return false, nil
	}
	for waited := time.Duration(0); ; waited += pollEvery {
		why, err := replacementInProgress(ctx, api)
		if err != nil {
			return false, err
		}
		if why == "" {
			break
		}
		if waited >= 2*WaitBack {
			return false, fmt.Errorf("%s — staying a plain container for now", why)
		}
		if !sleep(ctx, pollEvery) {
			return false, ctx.Err()
		}
	}
	if err := StartConvert(ctx, api, insp.Config.Image, out); err != nil {
		return false, err
	}
	return true, nil
}

// replacementInProgress says why an agent replacement is under way ("" when
// none is): a helper running, or an old agent still set aside.
func replacementInProgress(ctx context.Context, api swarm.API) (string, error) {
	if _, err := api.ContainerInspect(ctx, OldContainer); err == nil {
		return "a " + OldContainer + " container exists (an earlier replacement did not finish)", nil
	} else if !errdefs.IsNotFound(err) {
		return "", fmt.Errorf("checking for %s: %w", OldContainer, err)
	}
	h, err := api.ContainerInspect(ctx, HelperContainer)
	switch {
	case errdefs.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("checking for %s: %w", HelperContainer, err)
	case h.State != nil && h.State.Running:
		return HelperContainer + " is running", nil
	}
	return "", nil
}

// LeaderReplace is upgrade's last step on the leader: the leader service
// updated to img (Swarm stops this agent's task, starts the new one, and
// rolls back one that fails), or — a leader still in a plain container —
// converted straight onto img.
func LeaderReplace(ctx context.Context, api swarm.API, img string, out io.Writer) error {
	s, ok, err := leaderService(ctx, api)
	if err != nil {
		return err
	}
	if !ok {
		return StartConvert(ctx, api, img, out)
	}
	next := LeaderServiceSpec(img)
	// Always a new task: upgrade only gets here for another version, and a
	// rebuilt image under the same reference (homelab:dev) would otherwise
	// leave the spec unchanged — nothing for Swarm to roll out.
	next.TaskTemplate.ForceUpdate = s.Spec.TaskTemplate.ForceUpdate + 1
	if _, err := api.ServiceUpdate(ctx, s.ID, s.Version, next, dswarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("updating the %s service to %s: %w", AgentService, img, err)
	}
	_, _ = fmt.Fprintf(out, "agent restarting: the %s service is updated to %s\n", AgentService, img)
	return nil
}
