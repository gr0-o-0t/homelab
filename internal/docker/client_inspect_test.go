package docker_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dockerclient "github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/docker"
)

// containsPath checks if path ends with suffix — handles versioned API paths.
func containsPath(path, suffix string) bool {
	return strings.HasSuffix(path, suffix) || path == suffix
}

// newTestClient creates a docker.Client wired to a test HTTP server that
// handles /containers/{id}/json requests.
func newTestClient(t *testing.T, handler http.HandlerFunc) *docker.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost(srv.URL),
		dockerclient.WithHTTPClient(srv.Client()),
		dockerclient.WithAPIVersionNegotiation(),
	)
	require.NoError(t, err)

	// Use the constructor that accepts an existing SDK client.
	// We need to add NewWithClient — for now, construct directly.
	return docker.NewWithClient(c)
}

func TestInspectContainers_InspectFailure_KeepsSummary(t *testing.T) {
	summary := docker.ContainerSummary{
		ID:      docker.ShortID("xyz789"),
		Name:    "broken",
		Service: "broken-svc",
		State:   "exited",
		Status:  "Exited (1) 5 minutes ago",
		Image:   "alpine:latest",
	}

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container"}`))
	})

	ctx := context.Background()
	details, err := client.InspectContainers(ctx, []docker.ContainerSummary{summary})
	require.NoError(t, err)
	require.Len(t, details, 1)

	// When inspect fails, ContainerSummary fields should survive.
	d := details[0]
	assert.Equal(t, "xyz789", d.ID)
	assert.Equal(t, "broken", d.Name)
	assert.Equal(t, "broken-svc", d.Service)
	assert.Equal(t, "exited", d.State)
	assert.Equal(t, "Exited (1) 5 minutes ago", d.Status)
	assert.Equal(t, "alpine:latest", d.Image)
	// Detail fields should be zero-valued.
	assert.Equal(t, "", d.Health)
	assert.True(t, d.StartedAt.IsZero())
	assert.True(t, d.FinishedAt.IsZero())
	assert.Equal(t, 0, d.RestartCount)
	assert.Nil(t, d.Ports)
}

func TestInspectContainers_HappyPath(t *testing.T) {
	summary := docker.ContainerSummary{
		ID:   docker.ShortID("abc123def4567890"),
		Name: "myapp",
	}

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"Id": "abc123def4567890",
			"Name": "/myapp",
			"State": {
				"Status": "running",
				"Health": {"Status": "healthy"},
				"StartedAt": "2024-01-15T10:30:00Z",
				"FinishedAt": ""
			},
			"Config": {
				"Image": "nginx:latest",
				"Labels": {"com.docker.compose.service": "myapp"}
			},
			"HostConfig": {
				"PortBindings": {
					"80/tcp": [{"HostPort": "8080"}],
					"443/tcp": [{"HostPort": "8443"}]
				}
			},
			"NetworkSettings": {
				"Ports": {
					"80/tcp": [{"HostPort": "8080"}],
					"443/tcp": [{"HostPort": "8443"}]
				}
			},
			"RestartCount": 3
		}`))
	})

	ctx := context.Background()
	details, err := client.InspectContainers(ctx, []docker.ContainerSummary{summary})
	require.NoError(t, err)
	require.Len(t, details, 1)

	d := details[0]
	assert.Equal(t, "abc123def456", d.ID)
	assert.Equal(t, "myapp", d.Name)
	assert.Equal(t, "myapp", d.Service)
	assert.Equal(t, "running", d.State)
	assert.Equal(t, "healthy", d.Health)
	assert.Equal(t, 3, d.RestartCount)
	assert.NotZero(t, d.StartedAt, "started time should be parsed")
	assert.True(t, d.FinishedAt.IsZero(), "finished time should be zero when empty")
	assert.ElementsMatch(t, []string{"8080→80/tcp", "8443→443/tcp"}, d.Ports)
}

func TestInspectContainers_NoHealthcheck(t *testing.T) {
	summary := docker.ContainerSummary{
		ID:   docker.ShortID("nohc1234567890"),
		Name: "no-healthcheck",
	}

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"Id": "nohc1234567890",
			"Name": "/no-healthcheck",
			"State": {
				"Status": "running",
				"StartedAt": "",
				"FinishedAt": ""
			},
			"Config": {
				"Image": "alpine:latest",
				"Labels": {}
			},
			"HostConfig": {
				"PortBindings": null
			},
			"NetworkSettings": {
				"Ports": null
			},
			"RestartCount": 0
		}`))
	})

	ctx := context.Background()
	details, err := client.InspectContainers(ctx, []docker.ContainerSummary{summary})
	require.NoError(t, err)
	require.Len(t, details, 1)

	assert.Equal(t, "", details[0].Health, "health should be empty when no healthcheck configured")
	assert.Nil(t, details[0].Ports)
}

func TestInspectContainers_PortWithoutHostPort(t *testing.T) {
	summary := docker.ContainerSummary{
		ID:   docker.ShortID("portonly123456"),
		Name: "port-only",
	}

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"Id": "portonly123456",
			"Name": "/port-only",
			"State": {
				"Status": "running",
				"StartedAt": "",
				"FinishedAt": ""
			},
			"Config": {
				"Image": "alpine:latest",
				"Labels": {}
			},
			"HostConfig": {
				"PortBindings": {
					"3000/tcp": [{"HostPort": ""}]
				}
			},
			"NetworkSettings": {
				"Ports": {
					"3000/tcp": [{"HostPort": ""}]
				}
			},
			"RestartCount": 0
		}`))
	})

	ctx := context.Background()
	details, err := client.InspectContainers(ctx, []docker.ContainerSummary{summary})
	require.NoError(t, err)
	require.Len(t, details, 1)

	assert.Equal(t, []string{"3000/tcp"}, details[0].Ports)
}

func TestInspectContainers_MultipleContainers(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case containsPath(r.URL.Path, "/containers/abc123def456/json"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"Id": "abc123def4567890",
				"Name": "/alpha",
				"State": {
					"Status": "running",
					"Health": {"Status": "healthy"},
					"StartedAt": "2024-01-15T10:30:00Z",
					"FinishedAt": ""
				},
				"Config": {"Image": "nginx:latest", "Labels": {}},
				"HostConfig": {"PortBindings": {"80/tcp": [{"HostPort": "8080"}]}},
				"NetworkSettings": {"Ports": {"80/tcp": [{"HostPort": "8080"}]}},
				"RestartCount": 2
			}`))
		case containsPath(r.URL.Path, "/containers/xyz789deadbe/json"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"Id": "xyz789deadbeef",
				"Name": "/beta",
				"State": {
					"Status": "exited",
					"StartedAt": "2024-01-15T09:00:00Z",
					"FinishedAt": "2024-01-15T10:00:00Z"
				},
				"Config": {"Image": "alpine:latest", "Labels": {}},
				"HostConfig": {"PortBindings": {}},
				"NetworkSettings": {"Ports": {}},
				"RestartCount": 0
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such container"}`))
		}
	})

	ctx := context.Background()
	summaries := []docker.ContainerSummary{
		{ID: docker.ShortID("abc123def4567890"), Name: "alpha"},
		{ID: docker.ShortID("xyz789deadbeef"), Name: "beta"},
	}
	details, err := client.InspectContainers(ctx, summaries)
	require.NoError(t, err)
	require.Len(t, details, 2)

	assert.Equal(t, "alpha", details[0].Name)
	assert.Equal(t, "running", details[0].State)
	assert.Equal(t, "healthy", details[0].Health)

	assert.Equal(t, "beta", details[1].Name)
	assert.Equal(t, "exited", details[1].State)
	assert.Equal(t, "2024-01-15T09:00:00Z", details[1].StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	assert.Equal(t, "2024-01-15T10:00:00Z", details[1].FinishedAt.Format("2006-01-02T15:04:05Z07:00"))
}
