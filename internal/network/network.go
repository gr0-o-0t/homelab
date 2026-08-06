// Package network defines the NetworkLayer interface and Registry for managing
// homelab's network extension layers (tailscale, cloudflared, tor, i2p,
// yggdrasil). Each layer implements NetworkLayer and registers with
// the Registry, providing uniform lifecycle (Start/Stop/Status) and service
// exposure (Enable/Disable) regardless of the underlying implementation.
package network

import (
	"sync"
	"time"
)

// Status represents the current operational state of a network layer.
type Status struct {
	ContainerState string // "running", "stopped", "not found"
}

// EnvFunc supplies the environment for a layer's docker compose calls: root
// vars plus keyring secrets, i.e. cmd's buildEnv.
//
// A function rather than a map because it is called only when a layer actually
// shells out to compose. Building the map reads the system keyring, which can
// prompt for an unlock — not something `homelab ygg list` should trigger.
//
// Layers used to hardcode an empty map here. Compose then substituted "" for
// every variable, so `Layer.Start()` — a bare `--profile X up -d`, which
// targets the whole file — would recreate the tailscale container with a blank
// TS_AUTHKEY and drop the host off the tailnet.
type EnvFunc func() map[string]string

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
	Name     string // declaration key: "default", a listen port, or a subdomain
	Port     int    // container port number
	Listen   int    // site port clients connect on; 0 = the layer's default
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

	// ── Addressing ───────────────────────────────────────────────────────

	// ServiceAddresses returns every address a service answers on for this
	// layer, most canonical first, or nil when it is not exposed here.
	//
	// This lives on the layer because only the layer knows how its network
	// names things: tor reads the generated hostname file, i2p derives a b32
	// from the tunnel's destination key, ygg pairs the node address with the
	// allocated port, and the tailnet/Cloudflare layers template a hostname
	// out of env. Callers render what they get.
	//
	// It was previously a string-templating function in configgen shared by
	// nobody in particular, which is how `homelab status` came to advertise a
	// <name>.i2p host that resolves for no one and a ygg placeholder that
	// disagreed with `homelab ygg status`.
	//
	// Resolving may shell into a container, so callers listing many services
	// should expect it to be slow and cache per command, not per row.
	ServiceAddresses(svcName string, env map[string]string) []ServiceAddress

	// ── Config ───────────────────────────────────────────────────────────

	// CaddyConfigDir returns the conf.d-<ext> directory path for Caddy config
	// placement (e.g. "caddy/conf.d-tor" for tor).
	CaddyConfigDir(configRoot string) string
}

// ServiceAddress is one way to reach a service on a layer.
type ServiceAddress struct {
	// URL is what a client opens, e.g. "https://gitea.home.example.com" or
	// "http://abcd…xyz.b32.i2p".
	URL string

	// Note qualifies the URL when it needs qualifying — "needs an addressbook
	// entry", "node not running". Empty means the URL stands on its own.
	Note string
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

// ── Address caching ───────────────────────────────────────────────────────────

// AddressCache memoizes a slow address lookup.
//
// Resolving a tor/i2p/ygg address means shelling into a container, and the TUI
// asks for addresses on every render. Without this, opening a service's detail
// pane would run a `docker exec` per frame. The values it guards barely change:
// an onion address and an eepsite b32 are fixed for the life of the key, and a
// mesh address for the life of the node key.
//
// Zero value is ready to use. Safe for concurrent use: the TUI resolves from
// its render goroutine and its refresh commands.
type AddressCache struct {
	mu       sync.Mutex
	value    string
	resolved time.Time
}

// AddressCacheTTL is short enough that a container coming up is noticed within
// a few seconds, long enough that a redraw storm costs one lookup.
const AddressCacheTTL = 15 * time.Second

// Get returns the cached value, calling resolve when the cache is empty or
// stale. An empty result is not cached: it usually means "container not up
// yet", and that is precisely the state the caller wants to see change.
func (c *AddressCache) Get(resolve func() string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.value != "" && time.Since(c.resolved) < AddressCacheTTL {
		return c.value
	}
	c.value = resolve()
	c.resolved = time.Now()
	return c.value
}
