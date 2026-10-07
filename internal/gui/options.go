package gui

import (
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
)

// Options is everything the GUI needs from the CLI: how to list services, the
// network layers, the environment for address resolution, and the argv prefix
// that runs the homelab CLI. Every func is called off the render thread.
type Options struct {
	// Root is the config dir, for dynamic input choices (groups, backups).
	Root     string
	Discover func() ([]service.Service, error)
	// Layers are the configured layers: the private tailnet plus every
	// extension enabled in config.yaml. Re-read on every refresh.
	Layers func() []network.NetworkLayer
	// AllLayers is every layer the CLI knows, enabled or not.
	AllLayers []network.NetworkLayer
	Env       func(svc string) map[string]string
	CLI       []string
	// Core reports the state of each core container by name: caddy first,
	// then each layer's container.
	Core func() []ContainerState
}

// ContainerState is one core container and its docker state ("" = absent).
type ContainerState struct{ Name, State string }
