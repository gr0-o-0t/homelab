// Package docker wraps the Docker SDK client for read-only status queries.
// Lifecycle operations go through the Swarm API (internal/swarm), not here.
package docker

import (
	"context"
	"time"

	"github.com/docker/docker/api/types"
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
	Service string // compose key of the container in its stack
	State   string // "running" | "exited" | "created" | "restarting" | ...
	Status  string // Docker-formatted: "Up 3 hours", "Exited (0) 2 days ago"
	Image   string
	Node    string // Swarm node (hostname) running it; "" when not a Swarm task
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

// ContainerState returns the state ("running", "exited", etc.) of the named
// container (or Swarm service), or "" if it does not exist or the daemon is unavailable.
func (c *Client) ContainerState(ctx context.Context, name string) string {
	f := filters.NewArgs(filters.Arg("name", "^/"+name+"$"))
	list, err := c.c.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return ""
	}
	if len(list) == 0 {
		// A Swarm service's tasks are named <service>.<slot>.<id>: look the
		// name up as a service, preferring a running task.
		list, err = c.c.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
			filters.Arg("label", "com.docker.swarm.service.name="+name))})
		if err != nil || len(list) == 0 {
			return ""
		}
		for i := range list {
			if list[i].State == "running" {
				return "running"
			}
		}
	}
	return list[0].State
}

// Ping checks if the Docker daemon is reachable.
func (c *Client) Ping(ctx context.Context) (types.Ping, error) {
	return c.c.Ping(ctx)
}

// NetworkExists reports whether a Docker network with the given name exists.
func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	networks, err := c.c.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return false, err
	}
	for i := range networks {
		if networks[i].Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────
