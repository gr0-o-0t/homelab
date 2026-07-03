package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/service"
)

func detail(health string) docker.ContainerDetail {
	return docker.ContainerDetail{Health: health}
}

func TestAggregateHealth_Empty(t *testing.T) {
	assert.Equal(t, "", service.AggregateHealth(nil))
}

func TestAggregateHealth_SingleHealthy(t *testing.T) {
	assert.Equal(t, "healthy", service.AggregateHealth([]docker.ContainerDetail{detail("healthy")}))
}

func TestAggregateHealth_NoHealthcheck(t *testing.T) {
	assert.Equal(t, "", service.AggregateHealth([]docker.ContainerDetail{detail("")}))
}

// The whole point of this fix: an unrelated unhealthy sidecar must not be
// masked by a healthy main container, or vice versa — whichever container
// Docker's API happens to list first used to decide the entire service's
// displayed health.
func TestAggregateHealth_UnhealthySidecarWinsOverHealthyMain(t *testing.T) {
	containers := []docker.ContainerDetail{detail("healthy"), detail("unhealthy")}
	assert.Equal(t, "unhealthy", service.AggregateHealth(containers))

	// Order must not matter.
	reversed := []docker.ContainerDetail{detail("unhealthy"), detail("healthy")}
	assert.Equal(t, "unhealthy", service.AggregateHealth(reversed))
}

func TestAggregateHealth_StartingBeatsHealthyButNotUnhealthy(t *testing.T) {
	assert.Equal(t, "starting", service.AggregateHealth([]docker.ContainerDetail{detail("healthy"), detail("starting")}))
	assert.Equal(t, "unhealthy", service.AggregateHealth([]docker.ContainerDetail{detail("starting"), detail("unhealthy")}))
}

func TestAggregateHealth_HealthyIgnoresContainersWithNoHealthcheck(t *testing.T) {
	containers := []docker.ContainerDetail{detail("healthy"), detail("")}
	assert.Equal(t, "healthy", service.AggregateHealth(containers))
}
