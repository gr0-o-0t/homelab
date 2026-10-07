// Package network defines the NetworkLayer interface and Registry for
// homelab's network layers (tailscale, cloudflared, tor, i2p, yggdrasil).
// The layers themselves live in subpackages; internal/network/layers builds
// the registry, the only list of them.
package network

import (
	"strings"
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

// PortSelection is one resolved port of a service, as declared in its
// config.yaml (see config.PortEntry for the grammar).
type PortSelection struct {
	Name      string // declaration key: "default", a listen port, or a subdomain; "" for a routes-driven service
	Port      int    // container port traffic is forwarded to
	Listen    int    // site port clients connect on; 0 = the layer's default
	Subdomain string // replaces the service name in the site address; "" = service name
	Protocol  string // "tcp" or "udp"
}

// RoutableByCaddy reports whether this port can be served as a Caddy site.
// UDP cannot: Caddy speaks HTTP, and nothing in this stack proxies datagrams.
// Declaring 53/udp is still useful — compose publishes it — it just gets no
// site block instead of one that silently answers nothing.
func (p PortSelection) RoutableByCaddy() bool { return p.Protocol != "udp" }

// Site is one Caddy site block a layer serves for a service. A layer only says
// where the site lives; internal/configgen writes the block, so the choice
// between a service's caddy.routes.conf and a plain reverse_proxy is made in
// exactly one place for every layer.
type Site struct {
	PortName string // the port the file is named after (configgen.PortFileName); "" = <service>.conf
	Address  string // Caddy site address, e.g. "http://gitea.{$DOMAIN}" or ":9000"
	Port     int    // upstream container port a generated reverse_proxy targets
	TLS      bool   // import the wildcard_tls snippet (tailnet only)
	Comment  string // header written above the block, newline-terminated; may be empty
}

// NetworkLayer is one way a service can be reached: the tailnet, Cloudflare,
// Tor, I2P, Yggdrasil. Everything that differs between layers lives behind
// this interface, so adding one means writing a layer and registering it in
// internal/network/layers — not editing every command that lists layers.
type NetworkLayer interface {
	// ── Identity ─────────────────────────────────────────────────────────

	// Name returns the short identifier (e.g. "tor", "cf", "ts"), the
	// registry key.
	Name() string

	// Label returns a human-readable description (e.g. "Tor onion service proxy").
	Label() string

	// Flag returns the `homelab enable|disable --<flag>` that selects this
	// layer, or "" for the private tailnet layer, which is what the bare
	// commands act on.
	Flag() string

	// ContainerName returns the Docker container name for this layer.
	ContainerName() string

	// Profile returns the Docker Compose profile name for this layer, "" when
	// the container is part of the always-on core.
	Profile() string

	// ConfDir returns the directory under caddy/ holding this layer's
	// generated site blocks (e.g. "conf.d-tor"). The Caddyfile imports it.
	ConfDir() string

	// ── Lifecycle ────────────────────────────────────────────────────────

	// Start brings the layer's container(s) up.
	Start() error

	// Stop brings the layer's container(s) down.
	Stop() error

	// Status returns the current operational state.
	Status() Status

	// ── Routing ──────────────────────────────────────────────────────────

	// Sites returns the Caddy sites this layer serves for a service: where
	// each block lives and which upstream port it is for. host is the label
	// the service answers on (the service name, --name, or a declared
	// subdomain); ports are the service's resolved ports, or a single port
	// with an empty Name for a caddy.routes.conf service.
	//
	// Called after Configure, since some addresses (an onion, an allocated
	// mesh port) only exist once the layer's daemon side is set up.
	Sites(svcName, host string, ports []PortSelection) ([]Site, error)

	// ServiceAddresses returns every address a service answers on for this
	// layer, most canonical first, or nil when it is not exposed here.
	//
	// This lives on the layer because only the layer knows how its network
	// names things: tor reads the generated hostname file, i2p derives a b32
	// from the tunnel's destination key, ygg pairs the node address with the
	// allocated port, and the tailnet/Cloudflare layers template a hostname
	// out of env. Callers render what they get.
	//
	// Resolving may shell into a container, so callers listing many services
	// should expect it to be slow and cache per command, not per row.
	ServiceAddresses(svcName string, env map[string]string) []ServiceAddress
}

// Configurer is implemented by layers with daemon-side state per service — a
// torrc.d entry, an i2pd tunnel, a socat forwarder. Layers that are nothing but
// Caddy routing (the tailnet, Cloudflare) don't implement it.
type Configurer interface {
	// Configure sets the service up on the layer's daemon and reloads it.
	// displayName is the --name override, or the service name.
	Configure(svcName, displayName string, ports []PortSelection) error

	// Teardown removes the service from the layer's daemon and reloads it.
	// The Caddy blocks are removed by the caller.
	Teardown(svcName string) error
}

// HostRouted is implemented by layers whose site address is a template of the
// service's host label, so the label — including a --name it was enabled with —
// can be read back out of a generated block and the block regenerated without
// asking the layer's daemon anything.
type HostRouted interface {
	HostFromAddress(address string) (string, bool)
}

// HostTemplate is the site policy shared by the layers that route on the Host
// header (tailnet, Cloudflare, I2P): one site per Caddy-routable port, at
// Prefix + host + Suffix. Layers embed it.
type HostTemplate struct {
	Prefix, Suffix string
	TLS            bool
}

// Sites implements NetworkLayer.Sites for a Host-routed layer.
//
// UDP gets no site: Caddy speaks HTTP. Nor does a port with an explicit listen
// port (forgejo's 22:22): such a port is a raw TCP service published by
// compose, not HTTP. Wrapping it in a site block produced an HTTPS
// reverse_proxy on :22 (private) that no ssh client can speak, a plain-HTTP
// listener on :22 (cf) that cloudflared never routes to, and an eepsite block
// i2pd — which delivers only to :80 — could never reach.
func (h HostTemplate) Sites(_, host string, ports []PortSelection) ([]Site, error) {
	var sites []Site
	for _, p := range ports {
		if !p.RoutableByCaddy() || p.Listen != 0 {
			continue
		}
		label := host
		if p.Subdomain != "" {
			label = p.Subdomain // replaces the service name, never prefixes it
		}
		sites = append(sites, Site{
			PortName: p.Name, Address: h.Prefix + label + h.Suffix, Port: p.Port, TLS: h.TLS,
		})
	}
	return sites, nil
}

// HostFromAddress implements HostRouted.
func (h HostTemplate) HostFromAddress(address string) (string, bool) {
	addr := strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://")
	if i := strings.LastIndex(addr, "}:"); i >= 0 {
		addr = addr[:i+1] // drop a listen port
	}
	label, ok := strings.CutSuffix(addr, h.Suffix)
	return label, ok && label != ""
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

// Registry is an ordered set of network layers. internal/network/layers
// builds the one every command, the TUI and the GUI iterate.
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
