package docker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/docker"
)

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
