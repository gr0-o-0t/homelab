package diagnostics_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/diagnostics"
)

func TestRunConfigChecks_NoConfigFile(t *testing.T) {
	g := diagnostics.RunConfigChecks("/nonexistent/config.yaml")
	assert.NotEmpty(t, g.Results)
	// Should fail with config.yaml unreadable
	found := false
	for _, r := range g.Results {
		if r.Name == "config.yaml" {
			found = true
			assert.Equal(t, diagnostics.StatusFail, r.Status)
			break
		}
	}
	assert.True(t, found, "expected a config.yaml result")
}

func TestRunInfraChecks_NilClient(t *testing.T) {
	g := diagnostics.RunInfraChecks(nil)
	assert.NotEmpty(t, g.Results)
	assert.Equal(t, diagnostics.StatusFail, g.Results[0].Status)
}

func TestRunServiceConfigChecks_NonExistentService(t *testing.T) {
	g := diagnostics.RunServiceConfigChecks("/nonexistent", "nosuchservice")
	assert.NotEmpty(t, g.Results)
	// Should fail with services/<name>/ exists
	found := false
	for _, r := range g.Results {
		if r.Name == "nosuchservice" {
			found = true
			assert.Equal(t, diagnostics.StatusFail, r.Status)
			break
		}
	}
	assert.True(t, found, "expected a service directory check result")
}

func TestRunServiceContainerChecks_NilClient(t *testing.T) {
	g := diagnostics.RunServiceContainerChecks("test", nil, false)
	assert.NotEmpty(t, g.Results)
	assert.Equal(t, diagnostics.StatusWarn, g.Results[0].Status)
}

func TestRunServiceRoutingChecks_NonExistentService(t *testing.T) {
	g := diagnostics.RunServiceRoutingChecks("/nonexistent", "nosuchservice")
	assert.NotEmpty(t, g.Results)
	// Not exposed is a valid state: a warning, never a failure --fix "repairs".
	assert.Equal(t, diagnostics.StatusWarn, g.Results[0].Status)
}
