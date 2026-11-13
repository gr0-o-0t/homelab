// Package service handles service discovery and state.
package service

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
)

// Service represents a single entry under services/<name>/.
type Service struct {
	Name string
	Dir  string // absolute path to services/<name>/
	// HasCaddyConf is always false: static caddy.conf files are no longer
	// shipped or read. Kept only because internal/tui tests still set it.
	HasCaddyConf bool

	// Layers lists the network layers the service is exposed on, in registry
	// order — read from its exposure.yaml.
	Layers []LayerName

	Installed bool // true = exists on disk; false = catalog-only (not yet added)

	// Host port mappings from Docker inspect (e.g. "8080→8096/tcp").
	// Populated by DiscoverWith; empty when Docker is unavailable.
	HostPorts []string

	// Populated by DiscoverWith; zero-value when Docker is unavailable.
	// ContainerDetail (not just ContainerSummary) so per-container Health is
	// available without a second Docker round trip — see AggregateHealth.
	Containers []docker.ContainerDetail
	Running    int // containers (on Swarm: desired-running tasks) in state "running"
	Total      int // containers (on Swarm: tasks Swarm wants running)
	// StateUnknown: the state source could not be asked (Docker or Swarm
	// unreachable), so Running/Total say nothing.
	StateUnknown bool

	// Addresses are the service's addresses per layer it is on, as the CLI
	// reported them (internal/clistate); nil when discovered locally.
	Addresses map[string][]network.ServiceAddress

	// Ports are the ports its config.yaml declares, with what exposure.yaml
	// enables (configgen.PortStatuses); set by `homelab status`, which has
	// the env their addresses need.
	Ports []configgen.PortStatus
}

// AggregateHealth reduces a multi-container service's per-container health
// to one status: any "unhealthy" wins (something is actually broken); else
// any "starting" wins; else "healthy" if at least one container reports it
// (and none report unhealthy/starting); else "" (no healthcheck data at
// all). Using only the first container's health (as callers used to do)
// picks an arbitrary container's status to represent the whole service — a
// healthy app container could show "unhealthy" because of an unrelated
// sidecar, or vice versa.
func AggregateHealth(containers []docker.ContainerDetail) string {
	var sawUnhealthy, sawStarting, sawHealthy bool
	for i := range containers {
		switch containers[i].Health {
		case "unhealthy":
			sawUnhealthy = true
		case "starting":
			sawStarting = true
		case "healthy":
			sawHealthy = true
		}
	}
	switch {
	case sawUnhealthy:
		return "unhealthy"
	case sawStarting:
		return "starting"
	case sawHealthy:
		return "healthy"
	}
	return ""
}

// Discover scans the services/ directory and returns basic state without
// contacting the Docker daemon. Use DiscoverWith for live container data.
func Discover(repoRoot string) ([]Service, error) {
	return discover(repoRoot)
}

// StateSource reports the containers of an installed service: on Swarm, one
// row per task the service wants running, cluster-wide (swarm.Deployer).
type StateSource interface {
	ServiceContainers(ctx context.Context, name string) ([]docker.ContainerDetail, error)
}

// DiscoverWith scans services and enriches each with live container data
// from src. A service with no containers (not started, or src unreachable)
// simply has Running=0, Total=0.
func DiscoverWith(repoRoot string, src StateSource) ([]Service, error) {
	svcs, err := discover(repoRoot)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	enrich(ctx, src, svcs)
	return svcs, nil
}

// enrich populates Containers/Running/Total/HostPorts for each service from
// src, in place.
func enrich(ctx context.Context, src StateSource, svcs []Service) {
	for i := range svcs {
		svc := &svcs[i]
		containers, err := src.ServiceContainers(ctx, svc.Name)
		if err != nil {
			svc.StateUnknown = true // unreachable: counts say nothing
			continue
		}
		svc.Containers = containers
		svc.Total = len(containers)
		for ci := range containers {
			if containers[ci].State == "running" {
				svc.Running++
			}
			svc.HostPorts = append(svc.HostPorts, containers[ci].Ports...)
		}
	}
}

// ── internal ──────────────────────────────────────────────────────────────────

func discover(repoRoot string) ([]Service, error) {
	servicesDir := filepath.Join(repoRoot, "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var services []Service
	for _, e := range entries {
		// A dot dir is not a service: `files install` stages one there.
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		dir := filepath.Join(servicesDir, name)

		services = append(services, Service{
			Name:      name,
			Dir:       dir,
			Layers:    exposedLayers(repoRoot, name),
			Installed: true,
		})
	}

	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})
	return services, nil
}

// exposedLayers lists the layers name is exposed on, as its exposure.yaml
// records them, in registry order. A missing or unreadable file means none.
func exposedLayers(root, name string) []LayerName {
	st, err := exposure.Load(root, name)
	if err != nil {
		return nil
	}
	var out []LayerName
	for _, l := range layers.Static() {
		if st.On(l.Name()) {
			out = append(out, l.Name())
		}
	}
	return out
}

// ── Layer exposure ────────────────────────────────────────────────────────────

// LayerName is a network layer's short name, matching network.Registry keys.
type LayerName = string

// ActiveLayers returns the layers this service is currently exposed on, in
// display order.
func (s Service) ActiveLayers() []LayerName { return s.Layers }

// On reports whether the service is exposed on the named layer.
func (s Service) On(layer LayerName) bool {
	for _, l := range s.Layers {
		if l == layer {
			return true
		}
	}
	return false
}
