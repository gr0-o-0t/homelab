// Package configgen renders every generated Caddy file, for every network
// layer, from a service's declared ports or its caddy.routes.conf.
//
// A layer says where its sites live (network.NetworkLayer.Sites); Render wraps
// each in a block whose body is the service's routes file when it ships one,
// else a reverse_proxy to the site's port. That decision exists here only.
package configgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/network"
)

// protoTCP and protoUDP are the two protocols a declaration can carry.
const (
	protoTCP = "tcp"
	protoUDP = "udp"
)

// PortSelection is one resolved port to expose.
type PortSelection = network.PortSelection

// CaddyBlock is one generated Caddy file's content.
type CaddyBlock struct {
	PortName string // names the file (see PortFileName); "" for <service>.conf
	Content  string // Caddyfile snippet
}

// RoutesFileName is the optional per-service file holding the *body* of a Caddy
// site block — everything between the braces, with no site address and no TLS
// directive. It exists for services whose routing is more than one
// host → one upstream: AppFlowy splits eight path prefixes across five
// containers, and generating `reverse_proxy <svc>:<port>` for those left every
// route but "/" unreachable on the cf/i2p/tor/ygg layers.
//
// Being layer-agnostic is the point: the same body is wrapped in whichever site
// address a layer needs, so the routes are defined once instead of once per
// layer.
const RoutesFileName = "caddy.routes.conf"

// RoutesFile returns the path to a service's route snippet.
func RoutesFile(configDir, svcName string) string {
	return filepath.Join(configDir, "services", svcName, RoutesFileName)
}

// ServiceInfo holds the parsed service configuration needed for generation.
type ServiceInfo struct {
	Name    string
	Ports   config.PortEntries
	Routes  string // caddy.routes.conf body; empty when the service ships none
	HasVars bool   // whether config.yaml was found and parsed
}

// LoadServiceInfo reads a service's config.yaml and returns its port info.
// Returns an empty ServiceInfo (no error) if config.yaml doesn't exist.
func LoadServiceInfo(configDir, svcName string) (ServiceInfo, error) {
	info := ServiceInfo{Name: svcName, Ports: make(config.PortEntries)}

	if routes, err := os.ReadFile(RoutesFile(configDir, svcName)); err == nil {
		info.Routes = string(routes)
	}

	svcCfg, err := config.Load(config.ServiceConfigFile(configDir, svcName))
	if err != nil {
		return info, fmt.Errorf("loading config for %s: %w", svcName, err)
	}
	if svcCfg == nil {
		return info, nil
	}
	info.HasVars = true
	if svcCfg.Ports != nil {
		info.Ports = svcCfg.Ports
	}
	return info, nil
}

// ResolvePorts resolves the port selection from the --ports flag.
// If explicit port names are given, filter to those. Otherwise return all.
// For each resolved port, detect the protocol (tcp/udp) and assign a display name.
func ResolvePorts(ports config.PortEntries, selected []string) ([]PortSelection, error) {
	var result []PortSelection

	keys := make([]string, 0, len(ports))
	for k := range ports {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// No ports defined in config
	if len(keys) == 0 {
		return nil, fmt.Errorf("no ports defined in config.yaml for this service")
	}

	// If no selection, expose all
	if len(selected) == 0 {
		selected = keys
	}

	for _, name := range selected {
		entry, ok := ports[name]
		if !ok {
			return nil, fmt.Errorf("port %q not found in config.yaml ports section", name)
		}
		proto := protoTCP
		if !entry.HasTCP() {
			proto = protoUDP
		}
		result = append(result, PortSelection{
			Name:      name,
			Port:      entry.Port,
			Listen:    entry.Listen,
			Subdomain: entry.Subdomain,
			Protocol:  proto,
		})
	}
	return result, nil
}

// Exposure is what a service exposes, resolved once and handed to every layer
// it is enabled on.
type Exposure struct {
	Service string
	Host    string          // site host label: --name, a declared subdomain, or the service name
	Routes  string          // caddy.routes.conf body; "" for a port-driven service
	Ports   []PortSelection // resolved ports; a single unnamed one for a routes service
}

// Resolve reads a service's declaration into an Exposure. displayName is the
// --name override ("" or the service name for none); portNames restricts which
// declared ports are exposed (empty means all) and is ignored for a routes
// service, whose routing happens by path inside one block.
func Resolve(root, svcName, displayName string, portNames []string) (Exposure, error) {
	info, err := LoadServiceInfo(root, svcName)
	if err != nil {
		return Exposure{}, err
	}
	if info.Routes != "" {
		// One site per layer. A declared subdomain still applies to it, the
		// way vaultwarden serves vault.<home>.<domain> while keeping its
		// hand-written websocket and rate-limit directives. The port only
		// supplies the number the mesh layers record.
		return Exposure{
			Service: svcName,
			Host:    SiteHost(info, displayName),
			Routes:  info.Routes,
			Ports:   []PortSelection{{Port: PrimaryPort(info.Ports), Protocol: protoTCP}},
		}, nil
	}
	if len(info.Ports) == 0 {
		return Exposure{}, fmt.Errorf("no ports defined in config.yaml and no %s found for %s",
			RoutesFileName, svcName)
	}
	ports, err := ResolvePorts(info.Ports, portNames)
	if err != nil {
		return Exposure{}, err
	}
	if displayName == "" {
		displayName = svcName
	}
	return Exposure{Service: svcName, Host: displayName, Ports: ports}, nil
}

// Render produces every Caddy block a layer serves for an exposure. It does
// not write them; see WriteFile.
func Render(l network.NetworkLayer, e Exposure) ([]CaddyBlock, error) {
	sites, err := l.Sites(e.Service, e.Host, e.Ports)
	if err != nil {
		return nil, err
	}
	blocks := make([]CaddyBlock, 0, len(sites))
	for _, s := range sites {
		body := e.Routes
		if body == "" {
			body = fmt.Sprintf("reverse_proxy %s:%d\n", e.Service, s.Port)
		}
		var b strings.Builder
		b.WriteString(s.Comment)
		b.WriteString(s.Address + " {\n")
		if s.TLS {
			b.WriteString("\timport wildcard_tls\n")
		}
		b.WriteString(indentBody(stripLeadingComments(body)))
		b.WriteString("}\n")
		blocks = append(blocks, CaddyBlock{PortName: s.PortName, Content: b.String()})
	}
	return blocks, nil
}

// declaredSubdomain returns the subdomain a service declares, if it declares
// exactly one. More than one would need more than one site block, which a
// routes body cannot express.
func declaredSubdomain(ports config.PortEntries) string {
	found := ""
	for _, e := range ports {
		if e.Subdomain == "" {
			continue
		}
		if found != "" {
			return ""
		}
		found = e.Subdomain
	}
	return found
}

// SiteHost returns the host label — the part in front of the layer's domain —
// that a service's primary site answers on, resolved exactly as Generate
// resolves it: an explicit display name (--name) first, then a declared
// subdomain, then the service name. For a port-driven service the subdomain is
// the one on its primary HTTP port, since that port's block is the one a
// single-host layer reaches.
//
// Layers that have one name per service outside Caddy — the i2p tunnel's
// hostoverride, the Cloudflare DNS route — must use this rather than the bare
// service name, or the name they publish matches no site block.
//
// A displayName equal to the service name is treated as "no override": callers
// that default it (cmd/enable does) would otherwise mask a declared subdomain.
func SiteHost(info ServiceInfo, displayName string) string {
	if displayName == info.Name {
		displayName = ""
	}
	if info.Routes != "" {
		if displayName != "" {
			return displayName
		}
		if sub := declaredSubdomain(info.Ports); sub != "" {
			return sub
		}
		return info.Name
	}
	if p, ok := primaryHTTPPort(info.Ports); ok && p.Subdomain != "" {
		return p.Subdomain
	}
	if displayName != "" {
		return displayName
	}
	return info.Name
}

// primaryHTTPPort is the port a single-host layer lands on: "default" when it
// is Caddy-routable on the layer's own port, else the first such port by name.
func primaryHTTPPort(ports config.PortEntries) (PortSelection, bool) {
	resolved, err := ResolvePorts(ports, nil)
	if err != nil {
		return PortSelection{}, false
	}
	var first *PortSelection
	for i, p := range resolved {
		if !p.RoutableByCaddy() || p.Listen != 0 {
			continue
		}
		if p.Name == "default" {
			return p, true
		}
		if first == nil {
			first = &resolved[i]
		}
	}
	if first == nil {
		return PortSelection{}, false
	}
	return *first, true
}

// CurrentHost returns the host label a service's site answers on for a
// Host-routed layer (<label>.<DOMAIN> on Cloudflare, <label>.<home>.<DOMAIN> on
// the tailnet). The generated block is the source of truth — it records any
// --name the service was enabled with — so it is read first; without one, the
// host is resolved from the declaration as Resolve would.
func CurrentHost(root string, l network.NetworkLayer, svcName string) string {
	if host := GeneratedHost(root, l, svcName); host != "" {
		return host
	}
	return declaredHost(root, svcName)
}

// declaredHost resolves a service's host label from its declaration alone, as
// Generate would with no --name.
func declaredHost(configRoot, svcName string) string {
	info, err := LoadServiceInfo(configRoot, svcName)
	if err != nil {
		return svcName
	}
	return SiteHost(info, "")
}

// GeneratedHost reads the host label back out of a service's generated block
// for a layer — the one place a --name it was enabled with is recorded. It
// returns "" when the layer has no block for the service, or is not
// Host-routed, or the block's address is not one the layer wrote.
func GeneratedHost(root string, l network.NetworkLayer, svcName string) string {
	hr, ok := l.(network.HostRouted)
	if !ok {
		return ""
	}
	data, err := os.ReadFile(GeneratedFilePath(root, l.ConfDir(), svcName, ""))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if label, ok := hr.HostFromAddress(strings.TrimSpace(strings.TrimSuffix(line, "{"))); ok {
			return label
		}
		return ""
	}
	return ""
}

// PrimaryPort picks the port a routes-driven service should report to the
// network layers: the default/only one if there is one, else the
// lowest-numbered, else 80. The mesh layers proxy to Caddy and route on the
// Host header (i2pd writes `host = tailscale, port = 80` and ignores this), so it
// is bookkeeping rather than routing.
func PrimaryPort(ports config.PortEntries) int {
	if e, ok := ports["default"]; ok {
		return e.Port
	}
	if e, ok := ports["web"]; ok {
		return e.Port
	}
	best := 0
	for _, e := range ports {
		if best == 0 || e.Port < best {
			best = e.Port
		}
	}
	if best == 0 {
		return 80
	}
	return best
}

// stripLeadingComments drops the routes file's own header — the comment block
// before the first directive. That header documents the file (what it is, which
// layers wrap it) rather than the routing, and copying it into all five
// generated blocks buries the actual routes. Comments between directives are
// kept: those explain the routing and belong in the output.
func stripLeadingComments(body string) string {
	lines := strings.Split(body, "\n")

	// Only the first contiguous run of comment lines, stopping at the blank line
	// that ends it. Anything after that separator — including a comment
	// explaining the first route — is routing documentation and is kept.
	i := 0
	for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
		i++
	}
	if i == 0 {
		return body // starts with a directive; nothing to strip
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	return strings.Join(lines[i:], "\n")
}

// indentBody tab-indents a routes body one level, leaving blank lines bare.
// Caddy ignores indentation entirely; this is purely so the generated file
// reads like a hand-written site block.
func indentBody(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("\t" + line + "\n")
	}
	return b.String()
}

// HomeSubdomainVar is the Caddyfile placeholder for the home subdomain. Caddy
// expands it from the container environment; anything that writes a literal
// config file (i2pd's tunnels.conf) has to pass the resolved value instead.
const HomeSubdomainVar = "{$HOME_SUBDOMAIN}"

// I2PHost builds the host an eepsite answers on: <service>.<home>.i2p, the
// same segmentation as the tailnet name.
//
// Namespacing under the home subdomain is not cosmetic. A bare <service>.i2p
// is a name in the global I2P namespace that anyone can register, and requests
// for it go to whoever did — a browser asking for searxng.i2p reached a
// stranger's eepsite, because nothing publishes ours under that name and the
// router's addressbook had theirs.
//
// Both writers of this host must agree exactly or the eepsite 404s: Caddy
// matches the site address against the Host header, and i2pd sets that header
// from hostoverride. Hence one function, called from both, with the subdomain
// rendered as a placeholder for Caddy and as a literal for tunnels.conf.
func I2PHost(displayName, homeSubdomain string) string {
	if homeSubdomain == "" {
		return displayName + ".i2p"
	}
	return displayName + "." + homeSubdomain + ".i2p"
}

// ── File writing ───────────────────────────────────────────────────────────────

// ConfigDir returns a layer's Caddy config directory, given its
// network.NetworkLayer.ConfDir.
func ConfigDir(root, confDir string) string {
	return filepath.Join(root, "caddy", confDir)
}

// PortFileName returns the per-service, per-port basename used for every
// generated artifact: Caddy blocks here, socat forwarders in the ygg layer.
//
// A service with multiple non-default ports gets one file per port
// (<service>-<port>); the default/only port gets <service>. Every writer and
// every remover must agree on this, or enable writes files disable cannot
// find — which is why it is one exported function and not, as it was, the
// same six lines in configgen and in internal/network/ygg with a comment in
// each saying it mirrored the other.
func PortFileName(svcName, portName string) string {
	if portName != "" && portName != "default" && portName != "web" {
		return svcName + "-" + portName
	}
	return svcName
}

// GeneratedFilePath returns where a generated Caddy config block lands. Pass an
// empty portName for a routes-driven service, which has one file per layer.
func GeneratedFilePath(configRoot, confDir, svcName, portName string) string {
	return filepath.Join(ConfigDir(configRoot, confDir), PortFileName(svcName, portName)+".conf")
}

// WriteFile writes a generated Caddy config block to a layer's conf dir.
func WriteFile(configRoot, confDir, svcName, portName, content string) error {
	dir := ConfigDir(configRoot, confDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	return os.WriteFile(GeneratedFilePath(configRoot, confDir, svcName, portName), []byte(content), 0o600)
}

// RemoveFile removes a generated Caddy config file for the given service/layer/port.
func RemoveFile(configRoot, confDir, svcName, portName string) error {
	err := os.Remove(GeneratedFilePath(configRoot, confDir, svcName, portName))
	if os.IsNotExist(err) {
		return nil // already removed
	}
	return err
}

// RemoveAllPortFiles removes the generated Caddy config for every port the
// service declares under the given layer conf dir. Since a multi-port service
// gets one generated file per port (see PortFileName), removing only the
// default-named file (portName "") would orphan the rest. Falls back to a
// single default-name removal for services with no declared ports — that's
// the only file that could exist for them (and the name a symlink from the
// retired static-caddy.conf scheme would have, so disable still clears one).
func RemoveAllPortFiles(configRoot, confDir, svcName string) error {
	info, err := LoadServiceInfo(configRoot, svcName)
	// A routes-driven service has exactly one file per layer regardless of how
	// many ports it declares, so per-port removal would miss it.
	if err == nil && info.Routes != "" {
		return RemoveFile(configRoot, confDir, svcName, "")
	}
	if err != nil || len(info.Ports) == 0 {
		return RemoveFile(configRoot, confDir, svcName, "")
	}
	ports, err := ResolvePorts(info.Ports, nil)
	if err != nil {
		return RemoveFile(configRoot, confDir, svcName, "")
	}
	var firstErr error
	for _, p := range ports {
		if err := RemoveFile(configRoot, confDir, svcName, p.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
