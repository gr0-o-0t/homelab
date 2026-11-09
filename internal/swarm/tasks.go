package swarm

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/docker"
)

// Task is one task of a catalog service's stack that Swarm wants running,
// wherever in the cluster it is.
type Task struct {
	ID            string
	Service       string // Swarm service name, <svc>_<compose key>
	Key           string // compose key
	ContainerName string // compose container_name, "" when it has none
	Node          string // hostname of the node it is on (its ID when unknown)
	State         string // swarm task state: "running", "pending", "failed", …
	Err           string // why the task failed, when it did
	Image         string // image reference, without the pinned digest
	ContainerID   string // "" until the task has a container
	Since         time.Time
}

// Tasks lists the desired-running tasks of every Swarm service of svc,
// cluster-wide, by service name. A service scaled to zero has none.
func (d *Deployer) Tasks(ctx context.Context, svc string) ([]Task, error) {
	if err := config.ValidServiceName(svc); err != nil {
		return nil, err
	}
	list, err := d.services(ctx, svc)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	nodes, err := d.API.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	host := map[string]string{}
	for i := range nodes {
		host[nodes[i].ID] = nodes[i].Description.Hostname
	}
	var out []Task
	for i := range list {
		s := &list[i]
		tasks, err := d.API.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(
			filters.Arg("service", s.ID), filters.Arg("desired-state", string(swarm.TaskStateRunning)))})
		if err != nil {
			return nil, err
		}
		img := ""
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil {
			img, _, _ = strings.Cut(cs.Image, "@")
		}
		for j := range tasks {
			t := &tasks[j]
			node := host[t.NodeID]
			if node == "" {
				node = t.NodeID
			}
			task := Task{
				ID:            t.ID,
				Service:       s.Spec.Name,
				Key:           strings.TrimPrefix(s.Spec.Name, svc+"_"),
				ContainerName: s.Spec.Labels[LabelContainerName],
				Node:          node,
				State:         string(t.Status.State),
				Err:           t.Status.Err,
				Image:         img,
				Since:         t.Status.Timestamp,
			}
			if t.Status.ContainerStatus != nil {
				task.ContainerID = t.Status.ContainerStatus.ContainerID
			}
			out = append(out, task)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// ServiceContainers is svc's tasks as container rows (service.StateSource):
// state, node, image and start time from Swarm, health from the container
// when it runs on this node — the leader, where every pinned container is.
func (d *Deployer) ServiceContainers(ctx context.Context, svc string) ([]docker.ContainerDetail, error) {
	tasks, err := d.Tasks(ctx, svc)
	if err != nil {
		return nil, err
	}
	out := make([]docker.ContainerDetail, 0, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		name := t.ContainerName
		if name == "" {
			name = t.Service
		}
		status := t.State + " on " + t.Node
		if t.Err != "" {
			status += ": " + t.Err
		}
		c := docker.ContainerDetail{ContainerSummary: docker.ContainerSummary{
			ID:      docker.ShortID(t.ContainerID),
			Name:    name,
			Service: t.Key,
			State:   t.State,
			Status:  status,
			Image:   t.Image,
			Node:    t.Node,
		}}
		if t.State == string(swarm.TaskStateRunning) {
			c.StartedAt = t.Since
			c.Health = d.health(ctx, t.ContainerID)
		}
		out = append(out, c)
	}
	return out, nil
}

// health is a local container's health ("healthy", "unhealthy", "starting"),
// "" when it has no healthcheck or is not on this node.
func (d *Deployer) health(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	cs, err := d.API.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("id", id))})
	if err != nil || len(cs) == 0 {
		return ""
	}
	switch s := cs[0].Status; {
	case strings.Contains(s, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(s, "(healthy)"):
		return "healthy"
	case strings.Contains(s, "(health: starting)"):
		return "starting"
	}
	return ""
}
