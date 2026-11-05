package swarm_test

import (
	"encoding/json"
	"testing"

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
