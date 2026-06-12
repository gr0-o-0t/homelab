package diagnostics_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/diagnostics"
)

func TestCheckResult_LiteralConstruction(t *testing.T) {
	r := diagnostics.CheckResult{
		Name:    "Docker daemon",
		Status:  diagnostics.StatusPass,
		Message: "Docker daemon is running",
	}
	assert.Equal(t, diagnostics.StatusPass, r.Status)
	assert.Equal(t, "Docker daemon", r.Name)
}

func TestCheckResult_StatusConstants(t *testing.T) {
	assert.Equal(t, diagnostics.Status("pass"), diagnostics.StatusPass)
	assert.Equal(t, diagnostics.Status("warn"), diagnostics.StatusWarn)
	assert.Equal(t, diagnostics.Status("fail"), diagnostics.StatusFail)
}

func TestRunConfigChecks_NoConfigFile(t *testing.T) {
	g := diagnostics.RunConfigChecks("/nonexistent/config.yaml")
	assert.Equal(t, "Configuration", g.Title)
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
	assert.Equal(t, "Infrastructure", g.Title)
	assert.NotEmpty(t, g.Results)
	assert.Equal(t, diagnostics.StatusFail, g.Results[0].Status)
}

func TestRunCoreStackChecks_NilClient(t *testing.T) {
	g := diagnostics.RunCoreStackChecks(nil, "/nonexistent")
	assert.Equal(t, "Core Stack", g.Title)
	// No compose file should fail
	assert.NotEmpty(t, g.Results)
}

func TestRunServiceConfigChecks_NonExistentService(t *testing.T) {
	g := diagnostics.RunServiceConfigChecks("/nonexistent", "nosuchservice")
	assert.Equal(t, "Service Configuration", g.Title)
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
	g := diagnostics.RunServiceContainerChecks("test", nil)
	assert.Equal(t, "Service Containers", g.Title)
	assert.NotEmpty(t, g.Results)
	assert.Equal(t, diagnostics.StatusWarn, g.Results[0].Status)
}

func TestRunServiceRoutingChecks_NonExistentService(t *testing.T) {
	g := diagnostics.RunServiceRoutingChecks("/nonexistent", "nosuchservice")
	assert.Equal(t, "Service Routing", g.Title)
	assert.NotEmpty(t, g.Results)
	// Without dir, Caddy routes won't be active
	assert.Equal(t, diagnostics.StatusFail, g.Results[0].Status)
}

func TestCheckGroup_EmptyResults(t *testing.T) {
	g := diagnostics.CheckGroup{Title: "Empty", Results: nil}
	assert.Equal(t, "Empty", g.Title)
	assert.Empty(t, g.Results)
}
