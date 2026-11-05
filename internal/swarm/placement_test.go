package swarm

import (
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/groot/homelab/internal/config"
)

func svcv(v []types.ServiceVolumeConfig, name ...string) types.ServiceConfig {
	s := types.ServiceConfig{}
	s.Volumes = v
	if len(name) > 0 {
		s.Name = name[0]
	}
	return s
}

func TestPlace(t *testing.T) {
	named := func(src string) types.ServiceVolumeConfig {
		return types.ServiceVolumeConfig{Type: "volume", Source: src, Target: "/data"}
	}
	tests := []struct {
		name string
		svc  types.ServiceConfig
		cfg  *config.Config
		db   bool
		want Role
	}{
		{"stateless", types.ServiceConfig{}, &config.Config{}, false, RoleWorker},
		{"nil cfg", types.ServiceConfig{}, nil, false, RoleWorker},
		{"named volume", svcv([]types.ServiceVolumeConfig{named("data")}), &config.Config{}, false, RoleCore},
		{"cache-only volume", svcv([]types.ServiceVolumeConfig{named("cache")}), &config.Config{CacheVolumes: []string{"cache"}}, false, RoleWorker},
		{"cache and data volume", svcv([]types.ServiceVolumeConfig{named("cache"), named("data")}), &config.Config{CacheVolumes: []string{"cache"}}, false, RoleCore},
		{"bind", svcv([]types.ServiceVolumeConfig{{Type: "bind", Source: "./config", Target: "/config"}}), &config.Config{}, false, RoleCore},
		{"docker.sock", svcv([]types.ServiceVolumeConfig{{Type: "bind", Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}}), &config.Config{}, false, RoleCore},
		{"docker.sock target only", svcv([]types.ServiceVolumeConfig{{Type: "volume", Target: "/var/run/docker.sock"}}), &config.Config{}, false, RoleCore},
		{"db provider", types.ServiceConfig{}, &config.Config{}, true, RoleCore},
		{"override core", types.ServiceConfig{Name: "web"}, &config.Config{Placement: map[string]string{"web": "core"}}, false, RoleCore},
		{"override worker on data", svcv([]types.ServiceVolumeConfig{named("data")}, "web"), &config.Config{Placement: map[string]string{"web": "worker"}}, false, RoleCore},
		{"override any", types.ServiceConfig{Name: "web"}, &config.Config{Placement: map[string]string{"web": "any"}}, false, RoleAny},
		{"override worker", types.ServiceConfig{Name: "web"}, &config.Config{Placement: map[string]string{"web": "worker"}}, false, RoleWorker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Place(tt.svc, tt.cfg, tt.db); got != tt.want {
				t.Errorf("Place = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConstraints(t *testing.T) {
	tests := []struct {
		role  Role
		ready bool
		want  []string
	}{
		{RoleCore, false, []string{"node.labels.homelab.role==core"}},
		{RoleCore, true, []string{"node.labels.homelab.role==core"}},
		{RoleWorker, true, []string{"node.labels.homelab.role==worker"}},
		{RoleWorker, false, nil},
		{RoleAny, true, nil},
		{RoleAny, false, nil},
	}
	for _, tt := range tests {
		if got := Constraints(tt.role, tt.ready); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Constraints(%q,%v) = %v, want %v", tt.role, tt.ready, got, tt.want)
		}
	}
}
