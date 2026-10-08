package docker

import (
	"context"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
)

// Compose labels the session supervisor classifies containers by.
const (
	LabelProject    = "com.docker.compose.project"
	LabelWorkingDir = "com.docker.compose.project.working_dir"
	LabelService    = "com.docker.compose.service"
	LabelOneOff     = "com.docker.compose.oneoff"
)

// ContainerInfo is what the session supervisor needs to know about one
// compose-managed container: whose it is, whether Docker would start it at
// boot, when it started and what it bind-mounts from the host.
type ContainerInfo struct {
	ID            string // full ID
	Name          string // without the leading /
	Labels        map[string]string
	State         string    // "running", "exited", "created", …
	RestartPolicy string    // "no", "always", "unless-stopped", "on-failure"; "" counts as "no"
	StartedAt     time.Time // zero when Docker reported none
	BindSources   []string  // host paths of bind mounts
}

// Event is a container event, reduced to what the supervisor reads.
// Attributes carry the container's labels, its name and, on "die", exitCode.
type Event struct {
	Action     string // "start", "die", "kill", "oom", "destroy", …
	ID         string
	Attributes map[string]string
}

// ComposeContainers lists every container (any state) carrying a compose
// project label, inspected for restart policy, start time and bind mounts.
// Read-only: it changes nothing.
func (c *Client) ComposeContainers(ctx context.Context) ([]ContainerInfo, error) {
	list, err := c.c.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelProject)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(list))
	for i := range list {
		info, ok, err := c.InspectInfo(ctx, list[i].ID)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, info)
		}
	}
	return out, nil
}

// InspectInfo inspects one container. ok is false when it no longer exists.
func (c *Client) InspectInfo(ctx context.Context, id string) (ContainerInfo, bool, error) {
	j, err := c.c.ContainerInspect(ctx, id)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return ContainerInfo{}, false, nil
		}
		return ContainerInfo{}, false, err
	}
	info := ContainerInfo{ID: j.ID, Name: strings.TrimPrefix(j.Name, "/")}
	if j.Config != nil {
		info.Labels = j.Config.Labels
	}
	if j.State != nil {
		info.State = j.State.Status
		info.StartedAt = ParseTime(j.State.StartedAt)
	}
	if j.HostConfig != nil {
		info.RestartPolicy = string(j.HostConfig.RestartPolicy.Name)
	}
	for _, m := range j.Mounts {
		if m.Type == mount.TypeBind {
			info.BindSources = append(info.BindSources, m.Source)
		}
	}
	return info, true, nil
}

// ContainerEvents streams container events until ctx is done or the daemon
// connection breaks. The event channel is closed either way; on a broken
// connection the error channel yields the cause first.
func (c *Client) ContainerEvents(ctx context.Context) (<-chan Event, <-chan error) {
	msgs, errs := c.c.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(filters.Arg("type", string(events.ContainerEventType))),
	})
	out := make(chan Event)
	outErr := make(chan error, 1)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errs:
				if err != nil {
					outErr <- err
				}
				return
			case m := <-msgs:
				ev := Event{Action: string(m.Action), ID: m.Actor.ID, Attributes: m.Actor.Attributes}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, outErr
}

// PingOK reports whether the daemon answers.
func (c *Client) PingOK(ctx context.Context) error {
	_, err := c.c.Ping(ctx)
	return err
}
