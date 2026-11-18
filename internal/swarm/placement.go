// Package swarm holds the Docker Swarm side of homelab: where containers run.
package swarm

import (
	"slices"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/groot/homelab/internal/config"
)

// Role says which kind of node a container may run on.
type Role string

const (
	RoleCore   Role = "core"
	RoleWorker Role = "worker"
	RoleAny    Role = "any"
)

const dockerSock = "/var/run/docker.sock"

// Place decides where a compose service may run. Anything holding data, host
// paths, the docker socket or a shared database pins to core, and that wins
// over a placement override of worker.
// A pull_policy: never image (built locally) pins to core too, unless the
// service config names a placement. A global service runs on every node:
// its host paths and volumes are each node's own.
func Place(svc types.ServiceConfig, cfg *config.Config, isDBProvider bool) Role {
	if isDBProvider {
		return RoleCore
	}
	if IsGlobal(svc) {
		return RoleAny
	}
	var cache []string
	override := ""
	if cfg != nil {
		cache = cfg.CacheVolumes
		override = cfg.Placement[svc.Name]
	}
	for _, v := range svc.Volumes {
		switch {
		case v.Type == types.VolumeTypeBind,
			v.Source == dockerSock, v.Target == dockerSock,
			v.Type == types.VolumeTypeVolume && v.Source != "" && !slices.Contains(cache, v.Source):
			return RoleCore
		}
	}
	if svc.PullPolicy == types.PullPolicyNever && override == "" {
		return RoleCore // a locally built image exists on the leader only
	}
	switch override {
	case "core":
		return RoleCore
	case "any":
		return RoleAny
	}
	return RoleWorker
}

// Constraints returns the Swarm placement constraints for a role. Swarm has
// no soft affinity, so worker placement is only constrained once a worker is
// ready; until then the container runs on the minis.
func Constraints(r Role, workerReady bool) []string {
	switch r {
	case RoleCore:
		return []string{"node.labels.homelab.role==core"}
	case RoleWorker:
		if workerReady {
			return []string{"node.labels.homelab.role==worker"}
		}
	}
	return nil
}

// IsGlobal reports whether a compose service asks for one task per node
// (deploy.mode: global).
func IsGlobal(svc types.ServiceConfig) bool {
	return svc.Deploy != nil && svc.Deploy.Mode == "global"
}
