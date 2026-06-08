// Package docker wraps the Docker SDK client for read-only status queries.
// Lifecycle operations (up/down/restart) are still delegated to the
// docker compose CLI via internal/run to preserve Compose reconciliation logic.
package docker

import (
	"context"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
)

// Client wraps the Docker SDK client.
type Client struct {
	c *dockerclient.Client
}

// New creates a Client from the environment (respects DOCKER_HOST, TLS vars).
// The underlying connection is lazy — New() does not dial the daemon.
func New() (*Client, error) {
	c, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Close releases the underlying HTTP client.
func (c *Client) Close() error {
	return c.c.Close()
}

// NewWithClient wraps an existing Docker SDK client. Used in tests.
func NewWithClient(c *dockerclient.Client) *Client {
	return &Client{c: c}
}

// ShortID returns the first n characters of a container ID.
func ShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ParseTime parses a Docker RFC3339Nano timestamp.
func ParseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// ContainerSummary holds lightweight container data from ContainerList.
// Used for the service list view — no extra API calls needed per container.
type ContainerSummary struct {
	ID      string // first 12 chars
	Name    string // stripped of leading /
	Service string // com.docker.compose.service label
	State   string // "running" | "exited" | "created" | "restarting" | ...
	Status  string // Docker-formatted: "Up 3 hours", "Exited (0) 2 days ago"
	Image   string
}

// ContainerDetail holds the full data from ContainerInspect.
// Used for the `service ps` rich table.
type ContainerDetail struct {
	ContainerSummary
	Health       string    // "healthy" | "unhealthy" | "starting" | "" (no healthcheck)
	StartedAt    time.Time // zero if not running
	FinishedAt   time.Time // zero if still running
	RestartCount int
	Ports        []string // "8080→8080/tcp"
}

// ServiceContainers returns all containers (running or stopped) that belong
// to a Docker Compose project. The project name is the directory name of the
// service's docker-compose.yml, which Compose uses as the default project name.
func (c *Client) ServiceContainers(ctx context.Context, projectName string) ([]ContainerSummary, error) {
	f := filters.NewArgs(
		filters.Arg("label", "com.docker.compose.project="+projectName),
	)
	list, err := c.c.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return nil, err
	}

	out := make([]ContainerSummary, len(list))
	for i, ctr := range list {
		name := ""
		if len(ctr.Names) > 0 {
			name = strings.TrimPrefix(ctr.Names[0], "/")
		}
		out[i] = ContainerSummary{
			ID:      ShortID(ctr.ID),
			Name:    name,
			Service: ctr.Labels["com.docker.compose.service"],
			State:   ctr.State,
			Status:  ctr.Status,
			Image:   ctr.Image,
		}
	}
	return out, nil
}

// InspectContainers enriches a list of summaries with health, start time,
// restart count, and port bindings by calling ContainerInspect for each one.
func (c *Client) InspectContainers(ctx context.Context, summaries []ContainerSummary) ([]ContainerDetail, error) {
	out := make([]ContainerDetail, len(summaries))
	for i, s := range summaries {
		info, err := c.c.ContainerInspect(ctx, s.ID)
		if err != nil {
			// Keep the summary data even if inspect fails.
			out[i] = ContainerDetail{ContainerSummary: s}
			continue
		}

		health := ""
		if info.State.Health != nil {
			health = info.State.Health.Status
		}

		startedAt := ParseTime(info.State.StartedAt)
		finishedAt := ParseTime(info.State.FinishedAt)

		var ports []string
		for containerPort, bindings := range info.HostConfig.PortBindings {
			for _, b := range bindings {
				if b.HostPort != "" {
					ports = append(ports, b.HostPort+"→"+string(containerPort))
				} else {
					ports = append(ports, string(containerPort))
				}
			}
		}

		out[i] = ContainerDetail{
			ContainerSummary: ContainerSummary{
				ID:      ShortID(info.ID),
				Name:    strings.TrimPrefix(info.Name, "/"),
				Service: info.Config.Labels["com.docker.compose.service"],
				State:   info.State.Status,
				Status:  s.Status, // keep the pre-formatted Status from the list
				Image:   info.Config.Image,
			},
			Health:       health,
			StartedAt:    startedAt,
			FinishedAt:   finishedAt,
			RestartCount: info.RestartCount,
			Ports:        ports,
		}
	}
	return out, nil
}

// ContainerState returns the state ("running", "exited", etc.) of the named
// container, or "" if the container does not exist or the daemon is unavailable.
func (c *Client) ContainerState(ctx context.Context, name string) string {
	f := filters.NewArgs(filters.Arg("name", "^/"+name+"$"))
	list, err := c.c.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil || len(list) == 0 {
		return ""
	}
	return list[0].State
}

// NetworkExists reports whether a Docker network with the given name exists.
func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	networks, err := c.c.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return false, err
	}
	for _, n := range networks {
		if n.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────
