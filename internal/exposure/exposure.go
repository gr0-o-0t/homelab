// Package exposure stores which network layers a service is exposed on, and
// how: services/<name>/exposure.yaml.
//
// It is the only record of exposure. Discovery reads it for a service's
// layers; every Caddy site block is rendered from it plus the service's
// config.yaml. Before it existed, exposure was re-derived from which generated
// files happened to exist and from parsing them back (the --name out of a
// site address, mesh ports out of socat.d), so every reader had to agree with
// every writer on file names it never saw.
//
// Only `homelab enable` / `homelab disable` (through internal/routing), the
// ygg layer's mesh port allocation, and migration from the old per-layer files
// write it.
package exposure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// FileName is the state file's name inside a service directory.
const FileName = "exposure.yaml"

const header = "# Managed by `homelab enable` / `homelab disable`. Do not edit by hand.\n"

// State is one service's exposure.
type State struct {
	// Layers the service is exposed on, by registry name ("ts", "cf", …).
	Layers []string `yaml:"layers,flow"`
	// Name is the --name host label; empty means the service name.
	Name string `yaml:"name,omitempty"`
	// Ports is the --ports selection; empty means every declared port.
	Ports []string `yaml:"ports,omitempty,flow"`
	// YggPorts maps a port name ("default" for a routes-driven service's
	// single site) to its allocated Yggdrasil mesh port. Peers reach the
	// service as [node]:port, so an allocation must never move.
	YggPorts map[string]int `yaml:"ygg_ports,omitempty,flow"`
}

// Path returns where a service's state lives.
func Path(root, svc string) string {
	return filepath.Join(root, "services", svc, FileName)
}

// Load reads a service's state. A missing file is the zero State: not exposed.
func Load(root, svc string) (State, error) {
	var s State
	data, err := os.ReadFile(Path(root, svc)) //nolint:gosec // path built from the config dir
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parsing %s: %w", Path(root, svc), err)
	}
	return s, nil
}

// Save writes a service's state. A state with no layers and no mesh
// allocation is deleted rather than written: "not exposed" is the absence of
// the file, so a fully disabled service leaves nothing behind.
func Save(root, svc string, s State) error {
	path := Path(root, svc)
	if len(s.Layers) == 0 && len(s.YggPorts) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(header), data...), 0o600)
}

// Update loads a service's state, applies fn and saves the result.
func Update(root, svc string, fn func(*State)) error {
	s, err := Load(root, svc)
	if err != nil {
		return err
	}
	fn(&s)
	return Save(root, svc, s)
}

// On reports whether the service is exposed on a layer.
func (s State) On(layer string) bool { return slices.Contains(s.Layers, layer) }

// Set turns a layer on or off. Order is not significant: renderers and
// discovery iterate the layer registry.
func (s *State) Set(layer string, on bool) {
	switch {
	case on && !s.On(layer):
		s.Layers = append(s.Layers, layer)
	case !on:
		s.Layers = slices.DeleteFunc(s.Layers, func(l string) bool { return l == layer })
	}
}

// MeshPorts returns every mesh port allocated to any service but except, the
// set a new allocation must avoid.
func MeshPorts(root, except string) (map[int]bool, error) {
	matches, err := filepath.Glob(filepath.Join(root, "services", "*", FileName))
	if err != nil {
		return nil, err
	}
	taken := map[int]bool{}
	for _, m := range matches {
		svc := filepath.Base(filepath.Dir(m))
		if svc == except {
			continue
		}
		s, err := Load(root, svc)
		if err != nil {
			return nil, err
		}
		for _, p := range s.YggPorts {
			taken[p] = true
		}
	}
	return taken, nil
}

// MeshKey is the YggPorts key for a port: its declared name, or "default" for
// a routes-driven service's single unnamed site.
func MeshKey(portName string) string {
	if portName == "" {
		return "default"
	}
	return portName
}
