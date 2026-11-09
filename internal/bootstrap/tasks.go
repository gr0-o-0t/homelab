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

// StackService is one Swarm service of a catalog service's stack.
type StackService struct {
	Name          string // the Swarm service, <stack>_<key>
	Key           string // the compose key
	ContainerName string // the compose container_name, if any
}

func stackService(stack, name, containerName string) StackService {
	return StackService{Name: name, Key: strings.TrimPrefix(name, stack+"_"), ContainerName: containerName}
}

// PickService chooses the service exec acts on (and logs -c limits to).
// want (-c) names a compose key or container_name. Without it: the service
// keyed like the stack, then the one whose container_name is the stack's
// name (immich-server, container_name immich), then a stack's only
// service; otherwise it refuses and lists the choices.
func PickService(stack string, svcs []StackService, want string) (StackService, error) {
	if len(svcs) == 0 {
		return StackService{}, fmt.Errorf("%s: %w", stack, ErrNotRunning)
	}
	slices.SortFunc(svcs, func(a, b StackService) int { return cmp.Compare(a.Name, b.Name) })
	svcs = slices.CompactFunc(svcs, func(a, b StackService) bool { return a.Name == b.Name })
	if want != "" {
		for _, s := range svcs {
			if s.Key == want || s.ContainerName == want {
				return s, nil
			}
		}
		return StackService{}, fmt.Errorf("%s has no container %q — %s", stack, want, choices(svcs))
	}
	for _, s := range svcs {
		if s.Key == stack {
			return s, nil
		}
	}
	for _, s := range svcs {
		if s.ContainerName == stack {
			return s, nil
		}
	}
	if len(svcs) == 1 {
		return svcs[0], nil
	}
	return StackService{}, fmt.Errorf("%s has several containers; pick one with -c — %s", stack, choices(svcs))
}

func choices(svcs []StackService) string {
	var b strings.Builder
	b.WriteString("choose from: ")
	for i, s := range svcs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(s.Key)
		if s.ContainerName != "" && s.ContainerName != s.Key {
			b.WriteString(" (" + s.ContainerName + ")")
		}
	}
	return b.String()
}

// StackServices lists the Swarm services of stack svc (a manager only).
func StackServices(ctx context.Context, api swarm.API, svc string) ([]StackService, []string, error) {
	list, err := api.ServiceList(ctx, dswarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", swarm.LabelService+"="+svc))})
	if err != nil {
		return nil, nil, err
	}
	out := make([]StackService, 0, len(list))
	ids := make([]string, 0, len(list))
	for i := range list {
		out = append(out, stackService(svc, list[i].Spec.Name, list[i].Spec.Labels[swarm.LabelContainerName]))
		ids = append(ids, list[i].ID)
	}
	return out, ids, nil
}

// LocateTask decides where exec of svc (container want, see PickService)
// runs. local is true when this engine runs it itself: it is not a Swarm
// manager (a worker agent serving its own containers), or the task runs on
// this node. Otherwise hostname is the node running it. Engine errors are
// reported, never taken as local.
func LocateTask(ctx context.Context, api swarm.API, svc, want string) (hostname string, local bool, err error) {
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
	svcs := make([]StackService, 0, len(list))
	byName := map[string]string{}
	for i := range list {
		svcs = append(svcs, stackService(svc, list[i].Spec.Name, list[i].Spec.Labels[swarm.LabelContainerName]))
		byName[list[i].Spec.Name] = list[i].ID
	}
	s, err := PickService(svc, svcs, want)
	if err != nil {
		return "", false, err
	}
	tasks, err := api.TaskList(ctx, dswarm.TaskListOptions{Filters: filters.NewArgs(
		filters.Arg("service", byName[s.Name]), filters.Arg("desired-state", string(dswarm.TaskStateRunning)))})
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
	return "", false, fmt.Errorf("%s has no running task: %w", s.Name, ErrNotRunning)
}

// TaskContainer is the ID of the running task container on this engine of
// svc's service picked by want (PickService), the newest one while a
// rolling update has two. It needs no state dir: a worker agent finds it
// from the container labels alone, selected exactly by the stack label.
func TaskContainer(ctx context.Context, api swarm.API, svc, want string) (string, error) {
	list, err := api.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(
		filters.Arg("label", swarm.LabelNamespace+"="+svc), filters.Arg("status", "running"))})
	if err != nil {
		return "", fmt.Errorf("listing containers: %w", err)
	}
	svcs := make([]StackService, 0, len(list))
	for i := range list {
		l := list[i].Labels
		if l[SwarmServiceLabel] == "" {
			continue // not a Swarm task
		}
		svcs = append(svcs, stackService(svc, l[SwarmServiceLabel], l[swarm.LabelContainerName]))
	}
	s, err := PickService(svc, svcs, want)
	if err != nil {
		if errors.Is(err, ErrNotRunning) {
			return "", fmt.Errorf("%s has no running container on this node", svc)
		}
		return "", err
	}
	var mine []container.Summary
	for i := range list {
		if list[i].Labels[SwarmServiceLabel] == s.Name {
			mine = append(mine, list[i])
		}
	}
	newest := slices.MaxFunc(mine, func(a, b container.Summary) int { return cmp.Compare(a.Created, b.Created) })
	return newest.ID, nil
}
