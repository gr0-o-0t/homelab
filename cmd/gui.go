package cmd

import (
	"context"
	"time"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/gui"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
)

// runGUI opens the desktop GUI with the same inputs as the dashboard.
func runGUI(root string) error {
	dc, _ := docker.New()
	if dc != nil {
		defer func() { _ = dc.Close() }()
	}
	src, closeSrc := stateSource(root)
	defer closeSrc()
	catalog := catalogNames()
	all := extRegistry().All()
	return gui.Run(gui.Options{
		Root:      root,
		Discover:  func() ([]service.Service, error) { return discoverAll(root, src, catalog) },
		Layers:    func() []network.NetworkLayer { return uiLayers(root) },
		AllLayers: all,
		Env:       func(name string) map[string]string { return buildEnv(root, name) },
		CLI:       selfCLI(root),
		Core: func() []gui.ContainerState {
			if dc == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out := []gui.ContainerState{{Name: bootstrap.AgentContainer, State: dc.ContainerState(ctx, bootstrap.AgentContainer)}}
			for _, l := range all {
				if l.ContainerName() == bootstrap.AgentContainer {
					continue // the tailnet layer is the agent's
				}
				out = append(out, gui.ContainerState{Name: l.ContainerName(), State: dc.ContainerState(ctx, l.ContainerName())})
			}
			return out
		},
	})
}
