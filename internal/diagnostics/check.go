// Package diagnostics provides reusable health check types and functions
// consumed by both the CLI (cmd/) and TUI (internal/tui/).
package diagnostics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/service"
)

// Status represents the outcome of a single diagnostic check.
type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// CheckResult is a single diagnostic check result.
type CheckResult struct {
	Name    string // e.g. "Docker daemon", "Caddy config"
	Status  Status // pass / warn / fail
	Message string // human-readable detail
}

// CheckGroup is a named collection of checks.
type CheckGroup struct {
	Title   string
	Results []CheckResult
}

// RunConfigChecks validates the root config for required vars and secrets.
func RunConfigChecks(cfgFile string) CheckGroup {
	var results []CheckResult
	cfg, err := config.Load(cfgFile)
	if err != nil {
		results = append(results, CheckResult{
			Name: "config.yaml", Status: StatusFail,
			Message: fmt.Sprintf("config.yaml unreadable: %v", err),
		})
		return CheckGroup{Title: "Configuration", Results: results}
	}
	if cfg == nil {
		results = append(results, CheckResult{
			Name: "config.yaml", Status: StatusFail,
			Message: "config.yaml not found — run 'homelab setup'",
		})
		return CheckGroup{Title: "Configuration", Results: results}
	}
	results = append(results, CheckResult{
		Name: "config.yaml", Status: StatusPass, Message: "config.yaml readable",
	})

	sm, secretErr := secrets.Open()
	if secretErr != nil {
		results = append(results, CheckResult{
			Name: "keyring", Status: StatusWarn,
			Message: fmt.Sprintf("keyring unavailable: %v", secretErr),
		})
	}

	for k, e := range cfg.Vars {
		if e.Required {
			if e.Value == "" {
				results = append(results, CheckResult{
					Name: k, Status: StatusFail, Message: k + " is set",
				})
			} else {
				results = append(results, CheckResult{
					Name: k, Status: StatusPass, Message: k + " is set",
				})
			}
		}
	}
	for k, e := range cfg.Secrets {
		isSet := sm != nil && sm.IsSet("", k)
		if e.Required && !isSet {
			results = append(results, CheckResult{
				Name: k, Status: StatusFail, Message: k + " is set",
			})
		} else if e.Required {
			results = append(results, CheckResult{
				Name: k, Status: StatusPass, Message: k + " is set",
			})
		}
	}
	return CheckGroup{Title: "Configuration", Results: results}
}

// RunInfraChecks validates infrastructure dependencies (Docker, network).
func RunInfraChecks(dc *docker.Client) CheckGroup {
	var results []CheckResult

	if dc == nil {
		results = append(results, CheckResult{
			Name: "Docker", Status: StatusFail, Message: "Docker daemon is running",
		})
		return CheckGroup{Title: "Infrastructure", Results: results}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := dc.Ping(ctx)
	if err != nil {
		results = append(results, CheckResult{
			Name: "Docker", Status: StatusFail,
			Message: fmt.Sprintf("Docker daemon unreachable: %v", err),
		})
		return CheckGroup{Title: "Infrastructure", Results: results}
	}
	results = append(results, CheckResult{
		Name: "Docker", Status: StatusPass, Message: "Docker daemon is running",
	})

	netExists, _ := run.DockerNetworkExists("home-services")
	if netExists {
		results = append(results, CheckResult{
			Name: "home-services network", Status: StatusPass, Message: "Network 'home-services' exists",
		})
	} else {
		results = append(results, CheckResult{
			Name: "home-services network", Status: StatusFail, Message: "Network 'home-services' exists",
		})
	}

	return CheckGroup{Title: "Infrastructure", Results: results}
}

// RunServiceConfigChecks validates a service's config files and required vars.
func RunServiceConfigChecks(dir, name string) CheckGroup {
	var results []CheckResult

	svcDir := filepath.Join(dir, "services", name)
	if _, err := os.Stat(svcDir); os.IsNotExist(err) {
		results = append(results, CheckResult{
			Name: name, Status: StatusFail,
			Message: fmt.Sprintf("services/%s/ exists", name),
		})
		return CheckGroup{Title: "Service Configuration", Results: results}
	}
	results = append(results, CheckResult{
		Name: name, Status: StatusPass,
		Message: fmt.Sprintf("services/%s/ exists", name),
	})

	composeFile := run.ServiceComposeFile(dir, name)
	if _, err := os.Stat(composeFile); os.IsNotExist(err) {
		results = append(results, CheckResult{
			Name: "docker-compose.yml", Status: StatusFail,
			Message: fmt.Sprintf("services/%s/docker-compose.yml exists", name),
		})
	} else {
		results = append(results, CheckResult{
			Name: "docker-compose.yml", Status: StatusPass,
			Message: fmt.Sprintf("services/%s/docker-compose.yml exists", name),
		})
	}

	svcCfg, _ := config.Load(config.ServiceConfigFile(dir, name))
	if svcCfg != nil {
		sm, _ := secrets.Open()
		env, envErr := config.BuildEnv(
			config.RootConfigFile(dir, ""), dir, name, sm,
		)
		if envErr == nil && env != nil {
			for k, e := range svcCfg.Vars {
				if e.Required {
					if env[k] != "" {
						results = append(results, CheckResult{
							Name: k, Status: StatusPass, Message: k + " is set",
						})
					} else {
						results = append(results, CheckResult{
							Name: k, Status: StatusFail, Message: k + " is set",
						})
					}
				}
			}
			for k, e := range svcCfg.Secrets {
				isSet := sm != nil && sm.IsSet(name, k)
				if e.Required && !isSet {
					results = append(results, CheckResult{
						Name: k, Status: StatusFail, Message: k + " is set",
					})
				} else if e.Required {
					results = append(results, CheckResult{
						Name: k, Status: StatusPass, Message: k + " is set",
					})
				}
			}
		}
	}

	return CheckGroup{Title: "Service Configuration", Results: results}
}

// RunServiceContainerChecks checks a service's Swarm tasks, cluster-wide: all
// the tasks Swarm wants running are running, and the newest running one's
// image and health (health only for a container on this node). src nil means
// Docker is unreachable. stopped is the service's desired state: no tasks is
// then expected, a warning rather than a failure.
func RunServiceContainerChecks(name string, src service.StateSource, stopped bool) CheckGroup {
	const title = "Service Tasks"
	if src == nil {
		return CheckGroup{Title: title, Results: []CheckResult{{
			Name: "tasks", Status: StatusWarn, Message: "Docker unavailable",
		}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cs, err := src.ServiceContainers(ctx, name)
	if err != nil {
		return CheckGroup{Title: title, Results: []CheckResult{{
			Name: "tasks", Status: StatusFail, Message: fmt.Sprintf("listing tasks: %v", err),
		}}}
	}
	running := 0
	var newest *docker.ContainerDetail
	for i := range cs {
		if cs[i].State != "running" {
			continue
		}
		running++
		if newest == nil || cs[i].StartedAt.After(newest.StartedAt) {
			newest = &cs[i]
		}
	}
	r := CheckResult{Name: "tasks", Status: StatusPass,
		Message: fmt.Sprintf("%d/%d tasks running", running, len(cs))}
	switch {
	case len(cs) == 0:
		r.Status, r.Message = StatusFail, "no tasks scheduled — run homelab up "+name
		if stopped {
			r.Status, r.Message = StatusWarn, "no tasks scheduled — "+name+" is stopped"
		}
	case running < len(cs):
		r.Status = StatusFail
		for i := range cs {
			if cs[i].State != "running" {
				r.Message += fmt.Sprintf("; %s %s", cs[i].Name, cs[i].Status)
			}
		}
	}
	results := []CheckResult{r}
	if newest != nil {
		h := CheckResult{Name: newest.Name, Status: StatusPass,
			Message: fmt.Sprintf("%s on %s: %s", newest.Name, newest.Node, newest.Image)}
		switch newest.Health {
		case "unhealthy":
			h.Status, h.Message = StatusFail, h.Message+" (unhealthy)"
		case "starting":
			h.Status, h.Message = StatusWarn, h.Message+" (health: starting)"
		case "healthy":
			h.Message += " (healthy)"
		}
		results = append(results, h)
	}
	return CheckGroup{Title: title, Results: results}
}

// RunServiceRoutingChecks checks whether a service has active Caddy routes.
// RunServiceRoutingChecks reports which layers a service is exposed on.
//
// Not being exposed is a valid state — `homelab disable` exists to get there —
// so it is a warning, never a failure. It used to fail, and `doctor --fix`
// "repaired" it by re-enabling every deliberately disabled service. Exposure is
// read through service discovery, which reads services/<name>/exposure.yaml.
func RunServiceRoutingChecks(dir, name string) CheckGroup {
	var layers []string
	svcs, _ := service.Discover(dir)
	for i := range svcs {
		s := &svcs[i]
		if s.Name == name {
			for _, l := range s.ActiveLayers() {
				layers = append(layers, string(l))
			}
		}
	}
	r := CheckResult{Name: "Caddy routes", Status: StatusPass, Message: "exposed on " + strings.Join(layers, ", ")}
	if len(layers) == 0 {
		r.Status, r.Message = StatusWarn, "not exposed — run homelab enable "+name
	}
	return CheckGroup{Title: "Service Routing", Results: []CheckResult{r}}
}
