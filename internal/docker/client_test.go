package docker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/docker"
)

func TestContainerSummary_Fields(t *testing.T) {
	s := docker.ContainerSummary{
		ID:      "abc123def456",
		Name:    "myapp-server",
		Service: "myapp",
		State:   "running",
		Status:  "Up 2 hours",
		Image:   "nginx:latest",
	}

	assert.Equal(t, "abc123def456", s.ID)
	assert.Equal(t, "myapp-server", s.Name)
	assert.Equal(t, "myapp", s.Service)
	assert.Equal(t, "running", s.State)
	assert.Equal(t, "Up 2 hours", s.Status)
	assert.Equal(t, "nginx:latest", s.Image)
}

func TestContainerDetail_Fields(t *testing.T) {
	d := docker.ContainerDetail{
		ContainerSummary: docker.ContainerSummary{
			ID:      "abc123def456",
			Name:    "myapp-server",
			Service: "myapp",
			State:   "running",
		},
		Health:       "healthy",
		RestartCount: 3,
		Ports:        []string{"8080->8080/tcp", "3000"},
	}

	assert.Equal(t, "healthy", d.Health)
	assert.Equal(t, 3, d.RestartCount)
	assert.Len(t, d.Ports, 2)
}

func TestShortID_Truncates(t *testing.T) {
	longID := "abc123def456789012345678901234567890"
	result := docker.ShortID(longID)
	assert.Len(t, result, 12)
	assert.Equal(t, "abc123def456", result)
}

func TestShortID_PassthroughShort(t *testing.T) {
	shortID := "abc123"
	result := docker.ShortID(shortID)
	assert.Equal(t, "abc123", result)
}

func TestParseTime_Valid(t *testing.T) {
	s := "2024-01-15T10:30:00Z"
	tm := docker.ParseTime(s)
	assert.False(t, tm.IsZero())
}

func TestParseTime_Empty(t *testing.T) {
	tm := docker.ParseTime("")
	assert.True(t, tm.IsZero(), "empty string should return zero time")
}

func TestParseTime_Invalid(t *testing.T) {
	tm := docker.ParseTime("not-a-date")
	assert.True(t, tm.IsZero(), "invalid format should return zero time")
}

func TestParseTime_Nilish(t *testing.T) {
	tm := docker.ParseTime("0001-01-01T00:00:00Z")
	assert.True(t, tm.IsZero(), "zero time should be zero")
}