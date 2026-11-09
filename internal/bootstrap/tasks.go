package bootstrap

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/swarm"
)

// SwarmServiceLabel is the label Swarm puts on every task container: its
// service's name, <stack>_<compose key>.
const SwarmServiceLabel = "com.docker.swarm.service.name"

// ErrNotRunning means the service has no running task (container).
var ErrNotRunning = errors.New("not running")

// pickStackService chooses which of a stack's Swarm services exec/logs act
// on. The compose key is the exec/logs argument itself, so <svc>_<svc>
// wins; a stack with exactly one service needs no key. names are the
// stack's service names.
func pickStackService(svc string, names []string) (string, error) {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	main := svc + "_" + svc
	switch {
	case slices.Contains(names, main):
		return main, nil
	case len(names) == 1:
		return names[0], nil
	case len(names) == 0:
		return "", fmt.Errorf("%s: %w", svc, ErrNotRunning)
	}
	keys := make([]string, len(names))
	for i, n := range names {
		keys[i] = strings.TrimPrefix(n, svc+"_")
	}
	return "", fmt.Errorf("%s has several containers (%s) and none is named %s — exec/logs act on %s only",
		svc, strings.Join(keys, ", "), svc, main)
}

// LocateTask decides where exec/logs of svc run. local is true when this
// engine runs them itself: it is not a Swarm manager (a worker agent serving
// its own containers), or the task runs on this node. Otherwise hostname is
// the node running it. Engine errors are reported, never taken as local.
func LocateTask(ctx context.Context, api swarm.API, svc string) (hostname string, local bool, err error) {
	info, err := api.Info(ctx)
	if err != nil {
		return "", false, fmt.Errorf("asking Docker for this node's swarm role: %w", err)
	}
	if info.Swarm.LocalNodeState != dswarm.LocalNodeStateActive || !info.Swarm.ControlAvailable {
		return "", true, nil
	}
	list, err := api.ServiceList(ctx, dswarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", swarm.LabelService+"="+svc))})
	if err != nil {
		return "", false, err
	}
	byName := map[string]string{}
	names := make([]string, 0, len(list))
	for i := range list {
		byName[list[i].Spec.Name] = list[i].ID
		names = append(names, list[i].Spec.Name)
	}
	name, err := pickStackService(svc, names)
	if err != nil {
		return "", false, err
	}
	tasks, err := api.TaskList(ctx, dswarm.TaskListOptions{Filters: filters.NewArgs(
		filters.Arg("service", byName[name]), filters.Arg("desired-state", string(dswarm.TaskStateRunning)))})
	if err != nil {
		return "", false, err
	}
	for j := range tasks {
		t := &tasks[j]
		if t.Status.State != dswarm.TaskStateRunning {
			continue
		}
		if t.NodeID == info.Swarm.NodeID {
			return "", true, nil
		}
		nodes, err := api.NodeList(ctx, dswarm.NodeListOptions{})
		if err != nil {
			return "", false, err
		}
		for k := range nodes {
			if n := &nodes[k]; n.ID == t.NodeID && n.Description.Hostname != "" {
				return n.Description.Hostname, false, nil
			}
		}
		return "", false, fmt.Errorf("%s runs on node %s, whose hostname is unknown", svc, t.NodeID)
	}
	return "", false, fmt.Errorf("%s has no running task: %w", name, ErrNotRunning)
}

// TaskContainer is the ID of svc's running task container on this engine,
// the newest one while a rolling update has two. It needs no state dir: a
// worker agent finds it from the container labels alone.
func TaskContainer(ctx context.Context, api swarm.API, svc string) (string, error) {
	list, err := api.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(
		filters.Arg("label", SwarmServiceLabel), filters.Arg("status", "running"))})
	if err != nil {
		return "", fmt.Errorf("listing containers: %w", err)
	}
	var names []string
	for i := range list {
		if n := list[i].Labels[SwarmServiceLabel]; strings.HasPrefix(n, svc+"_") {
			names = append(names, n)
		}
	}
	name, err := pickStackService(svc, names)
	if err != nil {
		if errors.Is(err, ErrNotRunning) {
			return "", fmt.Errorf("%s has no running container on this node", svc)
		}
		return "", err
	}
	var mine []container.Summary
	for i := range list {
		if list[i].Labels[SwarmServiceLabel] == name {
			mine = append(mine, list[i])
		}
	}
	newest := slices.MaxFunc(mine, func(a, b container.Summary) int { return cmp.Compare(a.Created, b.Created) })
	return newest.ID, nil
}
