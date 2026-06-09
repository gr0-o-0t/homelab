// Package service handles service discovery and state.
package service

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/groot/homelab/internal/docker"
)

// Service represents a single entry under services/<name>/.
type Service struct {
	Name               string
	Dir                string // absolute path to services/<name>/
	HasCaddyConf       bool   // services/<name>/caddy.conf exists
	HasPublicCaddyConf bool   // services/<name>/caddy.cf.conf exists
	Enabled            bool   // caddy/conf.d/<name>.conf symlink is present (private)
	PublicEnabled      bool   // caddy/conf.d-cf/<name>.conf symlink is present (cf)
	Installed          bool   // true = exists on disk; false = catalog-only (not yet added)

	// Populated by DiscoverWithDocker; zero-value when Docker is unavailable.
	Containers []docker.ContainerSummary
	Running    int // count of containers in "running" state
	Total      int // total container count
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

	for i, svc := range svcs {
		containers, err := dc.ServiceContainers(ctx, svc.Name)
		if err != nil {
			// Docker unavailable or project not started — leave counts at zero.
			continue
		}
		svcs[i].Containers = containers
		svcs[i].Total = len(containers)
		for _, c := range containers {
			if c.State == "running" {
				svcs[i].Running++
			}
		}
	}
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

	for i, svc := range svcs {
		if !svc.Installed {
			continue
		}
		containers, err := dc.ServiceContainers(ctx, svc.Name)
		if err != nil {
			continue
		}
		svcs[i].Containers = containers
		svcs[i].Total = len(containers)
		for _, c := range containers {
			if c.State == "running" {
				svcs[i].Running++
			}
		}
	}
	return svcs, nil
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
			Name:               name,
			Dir:                dir,
			HasCaddyConf:       fileExists(filepath.Join(dir, "caddy.conf")),
			HasPublicCaddyConf: fileExists(filepath.Join(dir, "caddy.cf.conf")),
			Enabled:            symlinkExists(filepath.Join(repoRoot, "caddy", "conf.d", name+".conf")),
			PublicEnabled:      symlinkExists(filepath.Join(repoRoot, "caddy", "conf.d-cf", name+".conf")),
			Installed:          true,
		})
	}

	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})
	return services, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func symlinkExists(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}
