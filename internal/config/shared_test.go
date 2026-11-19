package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// "agent" is the leader agent's Swarm service (homelab_agent, labelled
// homelab.service=agent): a service of that name would be its stack, and
// `homelab down agent` would remove the leader.
func TestValidServiceName_AgentIsReserved(t *testing.T) {
	require.ErrorContains(t, ValidServiceName("agent"), "reserved")
	require.NoError(t, ValidServiceName("agent-x"))
}
