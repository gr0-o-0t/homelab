// Package clistate is what the front ends (dashboard, GUI) display, as the
// CLI reports it: `homelab status --json`, `homelab status <svc> --json` and
// `homelab backup --list --json`. Running those through this binary forwards
// them to the leader agent, so a front end shows the cluster from any PC — it
// never reads the local Docker engine or the local config dir.
package clistate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
)

// Node is one Swarm node.
type Node struct {
	Hostname     string `json:"hostname"`
	Role         string `json:"role"`         // homelab.role label: core or worker
	State        string `json:"state"`        // ready, down, …
	Availability string `json:"availability"` // active, pause, drain
	Manager      bool   `json:"manager"`
	Leader       bool   `json:"leader"`
}

// Extension is one optional network layer.
type Extension struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Container string `json:"container"` // its container or Swarm service
	Enabled   bool   `json:"enabled"`
	State     string `json:"state"` // "running", "not found", …
}

// Edge is the leader agent's Caddy.
type Edge struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

// Service is one installed service.
type Service struct {
	Name         string                              `json:"name"`
	Layers       []string                            `json:"layers,omitempty"`
	Running      int                                 `json:"running"`
	Total        int                                 `json:"total"`
	StateUnknown bool                                `json:"state_unknown,omitempty"`
	Containers   []docker.ContainerDetail            `json:"containers,omitempty"`
	Addresses    map[string][]network.ServiceAddress `json:"addresses,omitempty"`
}

// Status is `homelab status --json`.
type Status struct {
	Version string `json:"version"` // the agent's (the binary that ran status)
	Edge    Edge   `json:"edge"`
	// StateUnavailable: Docker/Swarm could not be asked; service states
	// are unknown.
	StateUnavailable bool        `json:"state_unavailable,omitempty"`
	Nodes            []Node      `json:"nodes"`
	Extensions       []Extension `json:"extensions"`
	Groups           []string    `json:"groups,omitempty"`
	Services         []Service   `json:"services"`
}

// FromService is s as reported, with its addresses per layer.
func FromService(s *service.Service, addrs map[string][]network.ServiceAddress) Service {
	return Service{
		Name: s.Name, Layers: slices.Clone(s.Layers), Running: s.Running, Total: s.Total,
		StateUnknown: s.StateUnknown, Containers: s.Containers, Addresses: addrs,
	}
}

// Service is s as the front ends' service model.
func (s Service) Service() service.Service {
	return service.Service{
		Name: s.Name, Installed: true, Layers: slices.Clone(s.Layers), Running: s.Running, Total: s.Total,
		StateUnknown: s.StateUnknown, Containers: s.Containers, Addresses: s.Addresses,
	}
}

// List is the installed services followed by the catalog entries not
// installed, each group sorted by name.
func (st Status) List(catalog []string) []service.Service {
	out := make([]service.Service, 0, len(st.Services)+len(catalog))
	installed := map[string]bool{}
	for _, s := range st.Services {
		out = append(out, s.Service())
		installed[s.Name] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	var rest []string
	for _, c := range catalog {
		if !installed[c] {
			rest = append(rest, c)
		}
	}
	sort.Strings(rest)
	for _, c := range rest {
		out = append(out, service.Service{Name: c})
	}
	return out
}

// Enabled is which extensions config.yaml enables, by name.
func (st Status) Enabled() map[string]bool {
	out := map[string]bool{}
	for _, e := range st.Extensions {
		out[e.Name] = e.Enabled
	}
	return out
}

// CoreStates is the state of each edge container by name: the agent (it
// runs Caddy and the tailnet node) "running" when its Caddy answers, then
// each extension's.
func (st Status) CoreStates(agent string) map[string]string {
	out := map[string]string{agent: "not running"}
	if st.Edge.Reachable {
		out[agent] = "running"
	}
	for _, e := range st.Extensions {
		if e.Container != "" {
			out[e.Container] = e.State
		}
	}
	return out
}

// Source is where a front end gets what it displays.
type Source interface {
	Status() (Status, error)
	Service(name string) (Service, error)
	Backups() ([]backup.Listing, error)
}

// CLI is the Source that runs this binary: Argv is its prefix (selfCLI).
type CLI struct {
	Argv []string
	// Run runs argv and returns its stdout; nil runs it with os/exec.
	Run func(argv []string) ([]byte, error)
}

func (c CLI) json(out any, args ...string) error {
	if len(c.Argv) == 0 {
		return errors.New("no homelab binary configured")
	}
	argv := append(slices.Clone(c.Argv), args...)
	run := c.Run
	if run == nil {
		run = execOutput
	}
	data, err := run(argv)
	if err != nil {
		return fmt.Errorf("homelab %s: %w", strings.Join(args, " "), err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("homelab %s: unreadable output: %w", strings.Join(args, " "), err)
	}
	return nil
}

func execOutput(argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- this binary plus fixed args
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			lines := strings.Split(msg, "\n")
			return nil, fmt.Errorf("%w: %s", err, lines[len(lines)-1])
		}
		return nil, err
	}
	return out, nil
}

// Status runs `homelab status --json`.
func (c CLI) Status() (Status, error) {
	var st Status
	err := c.json(&st, "status", "--json")
	return st, err
}

// Service runs `homelab status <name> --json`.
func (c CLI) Service(name string) (Service, error) {
	var s Service
	err := c.json(&s, "status", name, "--json")
	return s, err
}

// Backups runs `homelab backup --list --json`.
func (c CLI) Backups() ([]backup.Listing, error) {
	var l []backup.Listing
	err := c.json(&l, "backup", "--list", "--json")
	return l, err
}

// Choices returns the options of a Choice input: its static Choices, or its
// dynamic Source (groups, backups) as the leader reports it.
func Choices(src Source, in actions.Input) ([]string, error) {
	switch in.Source {
	case actions.SourceGroups:
		st, err := src.Status()
		return st.Groups, err
	case actions.SourceBackups:
		list, err := src.Backups()
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(list))
		for _, l := range list {
			out = append(out, l.Dir)
		}
		return out, nil
	}
	return in.Choices, nil
}
