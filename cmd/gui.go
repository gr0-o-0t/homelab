package cmd

import (
	"context"
	"time"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/gui"
	"github.com/groot/homelab/internal/service"
)

// runGUI opens the experimental desktop GUI with the same inputs as the
// dashboard.
func runGUI(root string) error {
	dc, _ := docker.New()
	if dc != nil {
		defer func() { _ = dc.Close() }()
	}
	catalog := catalogNames()
	layers := uiLayers(root)
	return gui.Run(gui.Options{
		Discover: func() ([]service.Service, error) { return discoverAll(root, dc, catalog) },
		Layers:   layers,
		Env:      func(name string) map[string]string { return buildEnv(root, name) },
		CLI:      selfCLI(root),
		Core: func() []gui.ContainerState {
			if dc == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out := []gui.ContainerState{{Name: "caddy", State: dc.ContainerState(ctx, "caddy")}}
			for _, l := range layers {
				out = append(out, gui.ContainerState{Name: l.ContainerName(), State: dc.ContainerState(ctx, l.ContainerName())})
			}
			return out
		},
	})
}
