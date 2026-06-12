// Package diagnostics provides reusable health check types and functions
// consumed by both the CLI (cmd/) and TUI (internal/tui/).
package diagnostics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/secrets"
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
				Name: k, Status: StatusFail, Message: k + " is set in keyring",
			})
		} else if e.Required {
			results = append(results, CheckResult{
				Name: k, Status: StatusPass, Message: k + " is set in keyring",
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

	_, tunErr := os.Stat("/dev/net/tun")
	if tunErr == nil {
		results = append(results, CheckResult{
			Name: "/dev/net/tun", Status: StatusPass, Message: "/dev/net/tun present",
		})
	} else {
		results = append(results, CheckResult{
			Name: "/dev/net/tun", Status: StatusWarn, Message: "/dev/net/tun not found (optional)",
		})
	}

	return CheckGroup{Title: "Infrastructure", Results: results}
}

// RunCoreStackChecks checks the core infrastructure containers.
func RunCoreStackChecks(dc *docker.Client, dir string) CheckGroup {
	var results []CheckResult

	if _, err := os.Stat(run.CoreComposeFile(dir)); os.IsNotExist(err) {
		results = append(results, CheckResult{
			Name: "core/docker-compose.yml", Status: StatusFail, Message: "core/docker-compose.yml present",
		})
		return CheckGroup{Title: "Core Stack", Results: results}
	}
	results = append(results, CheckResult{
		Name: "core/docker-compose.yml", Status: StatusPass, Message: "core/docker-compose.yml present",
	})

	if dc == nil {
		return CheckGroup{Title: "Core Stack", Results: results}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, name := range []string{"tailscale", "caddy"} {
		state := dc.ContainerState(ctx, name)
		r := CheckResult{Name: name + " container"}
		switch state {
		case "running":
			r.Status, r.Message = StatusPass, fmt.Sprintf("%s container %s", name, state)
		case "":
			r.Status, r.Message = StatusFail, fmt.Sprintf("%s container not found", name)
		default:
			r.Status, r.Message = StatusWarn, fmt.Sprintf("%s container %s", name, state)
		}
		results = append(results, r)
	}

	return CheckGroup{Title: "Core Stack", Results: results}
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
						Name: k, Status: StatusFail, Message: k + " is set in keyring",
					})
				} else if e.Required {
					results = append(results, CheckResult{
						Name: k, Status: StatusPass, Message: k + " is set in keyring",
					})
				}
			}
		}
	}

	return CheckGroup{Title: "Service Configuration", Results: results}
}

// RunServiceContainerChecks checks container state for a service.
func RunServiceContainerChecks(name string, dc *docker.Client) CheckGroup {
	var results []CheckResult

	if dc == nil {
		results = append(results, CheckResult{
			Name: "containers", Status: StatusWarn, Message: "Docker unavailable",
		})
		return CheckGroup{Title: "Service Containers", Results: results}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	summaries, err := dc.ServiceContainers(ctx, name)
	if err != nil || len(summaries) == 0 {
		results = append(results, CheckResult{
			Name: name, Status: StatusFail, Message: "no containers found",
		})
		return CheckGroup{Title: "Service Containers", Results: results}
	}

	for _, s := range summaries {
		if s.State == "running" {
			results = append(results, CheckResult{
				Name: s.Name, Status: StatusPass,
				Message: fmt.Sprintf("%s running", s.Name),
			})
		} else {
			results = append(results, CheckResult{
				Name: s.Name, Status: StatusFail,
				Message: fmt.Sprintf("%s %s", s.Name, s.State),
			})
		}
	}

	return CheckGroup{Title: "Service Containers", Results: results}
}

// RunServiceRoutingChecks checks whether a service has active Caddy routes.
func RunServiceRoutingChecks(dir, name string) CheckGroup {
	mgr := caddy.New(dir)
	enabled, _ := mgr.IsEnabled(name)
	pubEnabled, _ := mgr.IsPublicEnabled(name)

	var results []CheckResult
	if enabled || pubEnabled {
		results = append(results, CheckResult{
			Name: "Caddy routes", Status: StatusPass,
			Message: "at least one Caddy route active",
		})
	} else {
		results = append(results, CheckResult{
			Name: "Caddy routes", Status: StatusFail, Message: "at least one Caddy route active",
		})
	}

	return CheckGroup{Title: "Service Routing", Results: results}
}
