package styles

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_Dot_RunningEnabled(t *testing.T) {
	result := Dot(true, true)
	assert.NotEmpty(t, result)
}

func Test_Dot_RunningHidden(t *testing.T) {
	result := Dot(true, false)
	assert.NotEmpty(t, result)
}

func Test_Dot_Stopped(t *testing.T) {
	result := Dot(false, false)
	assert.NotEmpty(t, result)
}

func Test_HealthTag(t *testing.T) {
	assert.NotEmpty(t, HealthTag("healthy"))
	assert.NotEmpty(t, HealthTag("unhealthy"))
	assert.NotEmpty(t, HealthTag("starting"))
	assert.NotEmpty(t, HealthTag("unknown"))
}

func Test_StateTag(t *testing.T) {
	assert.NotEmpty(t, StateTag("running"))
	assert.NotEmpty(t, StateTag("exited"))
	assert.NotEmpty(t, StateTag("restarting"))
	assert.NotEmpty(t, StateTag("created"))
	assert.NotEmpty(t, StateTag("unknown"))
}

func Test_Pill(t *testing.T) {
	result := Pill("test", ColPrimary)
	assert.NotEmpty(t, result)
}

func Test_Width(t *testing.T) {
	s := Width(20)
	assert.NotNil(t, s)
}
