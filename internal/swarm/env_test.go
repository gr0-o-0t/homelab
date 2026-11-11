package swarm_test

import (
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types/container"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm"
)

func specWith(cmd, args []string) *dswarm.ServiceSpec {
	return &dswarm.ServiceSpec{TaskTemplate: dswarm.TaskSpec{
		ContainerSpec: &dswarm.ContainerSpec{Image: "x", Command: cmd, Args: args}}}
}

func TestWithEnv_SpecHasNoPlainEnv(t *testing.T) {
	s := specWith(nil, nil)
	swarm.WithEnv(s, "hl_x_env_abc", []string{"/entry"}, []string{"serve"})
	b, err := json.Marshal(s)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "hunter2")
	assert.Empty(t, s.TaskTemplate.ContainerSpec.Env)
	cs := s.TaskTemplate.ContainerSpec
	require.Len(t, cs.Secrets, 1)
	assert.Equal(t, "hl_x_env_abc", cs.Secrets[0].SecretName)
	assert.Equal(t, "homelab-env", cs.Secrets[0].File.Name)
	assert.EqualValues(t, 0o444, cs.Secrets[0].File.Mode)
	require.Len(t, cs.Mounts, 1)
	assert.Equal(t, "/hl", cs.Mounts[0].Target)
	assert.True(t, cs.Mounts[0].ReadOnly)
}

func TestWithEnv_CommandWrapsOriginal(t *testing.T) {
	s := specWith(nil, nil)
	swarm.WithEnv(s, "n", []string{"/entry"}, []string{"serve", "-v"})
	cs := s.TaskTemplate.ContainerSpec
	assert.Equal(t, []string{"/hl/homelab", "envshim", "--", "/entry", "serve", "-v"}, cs.Command)
	assert.Nil(t, cs.Args)
}

func TestWithEnv_ComposeCommandBeatsImage(t *testing.T) {
	s := specWith(nil, []string{"mine"})
	swarm.WithEnv(s, "n", []string{"/entry"}, []string{"imgcmd"})
	assert.Equal(t, []string{"/hl/homelab", "envshim", "--", "/entry", "mine"}, s.TaskTemplate.ContainerSpec.Command)

	s = specWith([]string{"/my-entry"}, nil) // compose entrypoint clears image cmd
	swarm.WithEnv(s, "n", []string{"/entry"}, []string{"imgcmd"})
	assert.Equal(t, []string{"/hl/homelab", "envshim", "--", "/my-entry"}, s.TaskTemplate.ContainerSpec.Command)
}

// Docker runs a healthcheck with the spec's env, which WithEnv keeps empty, so
// the test must go through the shim too (redis-cli needs REDISCLI_AUTH).
func TestWithEnv_HealthcheckThroughShim(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"CMD-SHELL", "redis-cli ping | grep -q PONG"},
			[]string{"CMD", "/hl/homelab", "envshim", "--", "/bin/sh", "-c", "redis-cli ping | grep -q PONG"}},
		{[]string{"CMD", "pg_isready", "-q"},
			[]string{"CMD", "/hl/homelab", "envshim", "--", "pg_isready", "-q"}},
		{[]string{"NONE"}, []string{"NONE"}},
	}
	for _, c := range cases {
		s := specWith(nil, nil)
		s.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{Test: c.in}
		swarm.WithEnv(s, "n", []string{"/entry"}, nil)
		assert.Equal(t, c.want, s.TaskTemplate.ContainerSpec.Healthcheck.Test)
	}

	s := specWith(nil, nil) // no healthcheck: the image's applies, left alone
	swarm.WithEnv(s, "n", []string{"/entry"}, nil)
	assert.Nil(t, s.TaskTemplate.ContainerSpec.Healthcheck)
}
