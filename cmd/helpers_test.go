package cmd

// White-box test — same package to access unexported helpers
// (formatUptime, truncate, buildServiceHint).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// ── formatUptime ──────────────────────────────────────────────────────────────

func TestFormatUptime_Minutes(t *testing.T) {
	assert.Equal(t, "0m", formatUptime(0))
	assert.Equal(t, "1m", formatUptime(90*time.Second))
	assert.Equal(t, "59m", formatUptime(59*time.Minute+30*time.Second))
}

func TestFormatUptime_Hours(t *testing.T) {
	assert.Equal(t, "1h 0m", formatUptime(time.Hour))
	assert.Equal(t, "2h 30m", formatUptime(2*time.Hour+30*time.Minute))
	assert.Equal(t, "23h 59m", formatUptime(23*time.Hour+59*time.Minute))
}

func TestFormatUptime_Days(t *testing.T) {
	assert.Equal(t, "1d 0h", formatUptime(24*time.Hour))
	assert.Equal(t, "3d 6h", formatUptime(3*24*time.Hour+6*time.Hour))
	assert.Equal(t, "365d 0h", formatUptime(365*24*time.Hour))
}

func TestFormatUptime_NegativeDuration(t *testing.T) {
	// Negative durations (clock skew) should be treated as positive.
	assert.Equal(t, "5m", formatUptime(-5*time.Minute))
}

// ── truncate ──────────────────────────────────────────────────────────────────

func TestTruncate_ShortString_Unchanged(t *testing.T) {
	assert.Equal(t, "hello", truncate("hello", 10))
	assert.Equal(t, "hello", truncate("hello", 5))
}

func TestTruncate_ExactLength_Unchanged(t *testing.T) {
	assert.Equal(t, "hello", truncate("hello", 5))
}

func TestTruncate_LongString_Truncated(t *testing.T) {
	result := truncate("hello world", 8)
	assert.Equal(t, "hello w…", result)
}

func TestTruncate_EmptyString(t *testing.T) {
	assert.Equal(t, "", truncate("", 10))
}

// ── buildServiceHint ──────────────────────────────────────────────────────────

func TestBuildServiceHint_EmptyList(t *testing.T) {
	hint := buildServiceHint(nil)
	assert.Contains(t, hint, "no services")
}

func TestBuildServiceHint_ListsServiceNames(t *testing.T) {
	svcs := []service.Service{
		{Name: "alpha"},
		{Name: "beta"},
		{Name: "gamma"},
	}
	hint := buildServiceHint(svcs)
	assert.Contains(t, hint, "alpha")
	assert.Contains(t, hint, "beta")
	assert.Contains(t, hint, "gamma")
}

// ── styles.Dot ────────────────────────────────────────────────────────────────
// Smoke-tests for the shared Dot helper — verifies it returns non-empty strings.

func TestDot_AllCombinations(t *testing.T) {
	cases := []struct{ running, exposed bool }{
		{true, true},
		{true, false},
		{false, true},
		{false, false},
	}
	for _, c := range cases {
		result := styles.Dot(c.running, c.exposed)
		assert.NotEmpty(t, result,
			"Dot(running=%v, exposed=%v) should never return empty string",
			c.running, c.exposed)
	}
}

// ── styles tag functions ──────────────────────────────────────────────────────

func TestHealthTag_KnownValues(t *testing.T) {
	cases := map[string]string{
		"healthy":   "healthy",
		"unhealthy": "unhealthy",
		"starting":  "starting",
		"":          "–",
		"unknown":   "–",
	}
	for input, wantContains := range cases {
		result := styles.HealthTag(input)
		assert.Contains(t, result, wantContains,
			"HealthTag(%q) should contain %q", input, wantContains)
	}
}

func TestStateTag_KnownValues(t *testing.T) {
	for _, state := range []string{"running", "exited", "restarting", "created", "paused"} {
		result := styles.StateTag(state)
		assert.NotEmpty(t, result, "StateTag(%q) should not be empty", state)
	}
}
