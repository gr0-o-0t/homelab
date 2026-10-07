// Package service handles service discovery and state.
package service

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/docker"
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
	// order — detected from its generated files in each layer's conf dir.
	Layers []LayerName

	Installed bool // true = exists on disk; false = catalog-only (not yet added)

	// Host port mappings from Docker inspect (e.g. "8080→8096/tcp").
	// Populated by DiscoverWithDocker; empty when Docker is unavailable.
	HostPorts []string

	// Populated by DiscoverWithDocker; zero-value when Docker is unavailable.
	// ContainerDetail (not just ContainerSummary) so per-container Health is
	// available without a second Docker round trip — see AggregateHealth.
	Containers []docker.ContainerDetail
	Running    int // count of containers in "running" state
	Total      int // total container count
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
// contacting the Docker daemon. Use DiscoverWithDocker for live container data.
func Discover(repoRoot string) ([]Service, error) {
	return discover(repoRoot)
}

// DiscoverWithDocker scans services and enriches each with live container
// data from the Docker daemon. Falls back gracefully if a service has no
// containers yet (not started) — it simply has Running=0, Total=0.
func DiscoverWithDocker(repoRoot string, dc *docker.Client) ([]Service, error) {
	svcs, err := discover(repoRoot)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	enrichWithDocker(ctx, dc, svcs, false)
	return svcs, nil
}

// DiscoverWithCatalog merges installed services with catalog-only entries.
// catalogNames lists names from the embedded catalog. Names already present on
// disk appear as normal installed services; the remainder appear as catalog-only
// stubs (Installed=false, no Dir, no container data). Installed services sort
// first, both groups sorted alphabetically within themselves.
func DiscoverWithCatalog(repoRoot string, catalogNames []string) ([]Service, error) {
	installed, err := discover(repoRoot)
	if err != nil {
		return nil, err
	}

	installedSet := make(map[string]bool, len(installed))
	for _, s := range installed {
		installedSet[s.Name] = true
	}

	for _, name := range catalogNames {
		if installedSet[name] {
			continue
		}
		installed = append(installed, Service{Name: name, Installed: false})
	}

	sort.Slice(installed, func(i, j int) bool {
		if installed[i].Installed != installed[j].Installed {
			return installed[i].Installed // installed before catalog-only
		}
		return installed[i].Name < installed[j].Name
	})
	return installed, nil
}

// DiscoverAllWithDocker is like DiscoverWithCatalog but enriches installed
// services with live Docker container data. Catalog-only entries are skipped.
func DiscoverAllWithDocker(repoRoot string, dc *docker.Client, catalogNames []string) ([]Service, error) {
	svcs, err := DiscoverWithCatalog(repoRoot, catalogNames)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	enrichWithDocker(ctx, dc, svcs, true)
	return svcs, nil
}

// enrichWithDocker populates Containers/Running/Total/HostPorts for each
// service from live Docker data, in place. Shared by DiscoverWithDocker and
// DiscoverAllWithDocker, which previously duplicated this loop verbatim.
// onlyInstalled skips catalog-only stub entries (DiscoverAllWithDocker's
// case); DiscoverWithDocker's services are always installed, so it passes
// false.
func enrichWithDocker(ctx context.Context, dc *docker.Client, svcs []Service, onlyInstalled bool) {
	for i, svc := range svcs {
		if onlyInstalled && !svc.Installed {
			continue
		}
		containers, err := dc.ServiceContainers(ctx, svc.Name)
		if err != nil {
			// Docker unavailable or project not started — leave counts at zero.
			continue
		}
		svcs[i].Total = len(containers)
		for _, c := range containers {
			if c.State == "running" {
				svcs[i].Running++
			}
		}

		details, err := dc.InspectContainers(ctx, containers)
		if err != nil {
			// Fall back to bare summaries so Total/Running/State still
			// reflect reality even without Health/Ports.
			details = make([]docker.ContainerDetail, len(containers))
			for j, c := range containers {
				details[j] = docker.ContainerDetail{ContainerSummary: c}
			}
		}
		svcs[i].Containers = details
		for di := range details {
			svcs[i].HostPorts = append(svcs[i].HostPorts, details[di].Ports...)
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
		if !e.IsDir() {
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

// exposedLayers lists the layers name has a generated block in.
func exposedLayers(root, name string) []LayerName {
	var out []LayerName
	for _, l := range layers.New(root, nil, nil).All() {
		if exposedOn(root, l.ConfDir(), name) {
			out = append(out, l.Name())
		}
	}
	return out
}

// exposedOn reports whether any generated block for name exists in a layer's
// conf dir. Checking only <name>.conf missed services whose ports are named (a
// subdomain port lands in <name>-<port>.conf); the file names are computed
// from the declared ports rather than globbed, because "<name>-*" would also
// match a different service called "<name>-something".
func exposedOn(root, confDir, name string) bool {
	files := []string{configgen.GeneratedFilePath(root, confDir, name, "")}
	if info, err := configgen.LoadServiceInfo(root, name); err == nil {
		if ports, err := configgen.ResolvePorts(info.Ports, nil); err == nil {
			for _, p := range ports {
				files = append(files, configgen.GeneratedFilePath(root, confDir, name, p.Name))
			}
		}
	}
	for _, f := range files {
		if fileExists(f) {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
