// Package network defines the NetworkLayer interface and Registry for managing
// homelab's network extension layers (tailscale, cloudflared, tor, i2p,
// yggdrasil, ipfs). Each layer implements NetworkLayer and registers with
// the Registry, providing uniform lifecycle (Start/Stop/Status) and service
// exposure (Enable/Disable) regardless of the underlying implementation.
package network

// Status represents the current operational state of a network layer.
type Status struct {
	ContainerState string // "running", "stopped", "not found"
}

// ServiceInfo holds parsed service configuration for Enable/Disable operations.
// Layers use this to generate Caddy blocks and tunnel configs.
// Ports maps port name → port number; Protocol is carried in PortSelection.
type ServiceInfo struct {
	Name    string
	Ports   map[string]int // port name → port number
	HasVars bool
}

// PortSelection describes a single exposed port for a service.
type PortSelection struct {
	Name     string // "web", "ssh", or "default"
	Port     int    // container port number
	Protocol string // "tcp" or "udp"
}

// NetworkLayer defines the interface that every network extension must implement.
// Each layer provides:
//   - Identity (Name, Label, ContainerName, Profile) for CLI display and lookup
//   - Lifecycle (Start, Stop, Status) for Docker Compose management
//   - Service exposure (Enable, Disable) for per-service routing configuration
//   - Config directory (CaddyConfigDir) for Caddy conf.d-<ext>/ placement
type NetworkLayer interface {
	// ── Identity ─────────────────────────────────────────────────────────

	// Name returns the short identifier (e.g. "tor", "cf", "ts").
	Name() string

	// Label returns a human-readable description (e.g. "Tor onion service proxy").
	Label() string

	// ContainerName returns the Docker container name for this layer.
	ContainerName() string

	// Profile returns the Docker Compose profile name for this layer.
	// Used during startup to activate the correct compose profiles.
	Profile() string

	// ── Lifecycle ────────────────────────────────────────────────────────

	// Start brings the layer's container(s) up.
	Start() error

	// Stop brings the layer's container(s) down.
	Stop() error

	// Status returns the current operational state.
	Status() Status

	// ── Service exposure ─────────────────────────────────────────────────

	// Enable configures Caddy routing AND extension-specific tunnel config
	// for the given service. Writes Caddy config to CaddyConfigDir() and
	// extension-specific config (torrc.d, tunnels.conf, socat.d, etc.).
	Enable(svcName, displayName string, info ServiceInfo, ports []PortSelection) error

	// Disable removes extension-specific routing for the given service.
	// Removes Caddy config from CaddyConfigDir() AND extension-specific config.
	Disable(svcName string) error

	// ── Config ───────────────────────────────────────────────────────────

	// CaddyConfigDir returns the conf.d-<ext> directory path for Caddy config
	// placement (e.g. "caddy/conf.d-tor" for tor).
	CaddyConfigDir(configRoot string) string
}

// ── Registry ──────────────────────────────────────────────────────────────────

// Registry manages the set of registered network layers. Layers register
// themselves during init() or programmatic setup, and the CLI/TUI iterate
// the registry for lifecycle commands and status display.
type Registry struct {
	layers map[string]NetworkLayer
	order  []string // insertion order for deterministic iteration
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		layers: make(map[string]NetworkLayer),
	}
}

// Register adds a layer to the registry. Panics on duplicate name.
func (r *Registry) Register(layer NetworkLayer) {
	name := layer.Name()
	if _, dup := r.layers[name]; dup {
		panic("network: duplicate registration for layer " + name)
	}
	r.layers[name] = layer
	r.order = append(r.order, name)
}

// Get returns a registered layer by name.
func (r *Registry) Get(name string) (NetworkLayer, bool) {
	l, ok := r.layers[name]
	return l, ok
}

// All returns all registered layers in registration order.
func (r *Registry) All() []NetworkLayer {
	result := make([]NetworkLayer, 0, len(r.order))
	for _, name := range r.order {
		result = append(result, r.layers[name])
	}
	return result
}

// Names returns the names of all registered layers in registration order.
func (r *Registry) Names() []string {
	names := make([]string, len(r.order))
	copy(names, r.order)
	return names
}

// Has reports whether a layer with the given name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.layers[name]
	return ok
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// Compile-time check that a value satisfies the interface.
// Layer packages use this in their own compilation unit:
//
//	var _ network.NetworkLayer = (*Layer)(nil)
