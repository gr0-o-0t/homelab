package gui

import (
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
)

// Options is everything the GUI needs from the CLI: how to list services, the
// configured layers, the environment for address resolution, and the argv
// prefix that runs the homelab CLI.
type Options struct {
	Discover func() ([]service.Service, error)
	Layers   []network.NetworkLayer
	Env      func(svc string) map[string]string
	CLI      []string
}
