// Package configgen generates Caddy config blocks from service port declarations
// and extension flags. Used by `homelab enable <service> --cf --i2p ...`.
//
// This is the modern routing path alongside the legacy symlink path in
// caddy.Manager. New services declare ports in config.yaml and this package
// generates Caddy config directly. Old services ship static caddy.conf files
// that caddy.Manager symlinks into place.
package configgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/config"
)

// protoTCP and protoUDP are the two protocols a declaration can carry.
const (
	protoTCP = "tcp"
	protoUDP = "udp"

	// extI2P is named because the i2p layer is the one with rules the others
	// don't share: a home-subdomain-namespaced host, and no explicit ports.
	extI2P = "i2p"
)

// PortSelection is one resolved port to expose.
type PortSelection struct {
	Name      string // declaration key: "default", a listen port, or a subdomain
	Port      int    // container port traffic is forwarded to
	Listen    int    // site port clients connect on; 0 = the layer's default
	Subdomain string // replaces the service name in the site address; "" = service name
	Protocol  string // "tcp" or "udp"
}

// RoutableByCaddy reports whether this port can be served as a Caddy site.
// UDP cannot: Caddy speaks HTTP, and nothing in this stack proxies datagrams.
// Declaring 53/udp is still useful — compose publishes it — it just gets no
// site block instead of one that silently answers nothing.
func (p PortSelection) RoutableByCaddy() bool { return p.Protocol != protoUDP }

// CaddyBlock is one generated Caddy config block.
type CaddyBlock struct {
	Extension string // "private", "cf", "i2p", "tor", "ygg"
	PortName  string // port name ("" for a routes-driven block)
	Port      int    // upstream container port, for network-layer bookkeeping
	Listen    int    // declared listen port, 0 when the layer's default applies
	Content   string // Caddyfile snippet
}

// Request collects inputs for config generation.
type Request struct {
	ServiceName string   // service directory name (e.g., "gitea")
	DisplayName string   // --name override (defaults to ServiceName)
	Extensions  []string // selected extensions ("cf", "i2p", "tor", "ygg")
	PortNames   []string // selected port names --ports flag (empty = all)
	ConfigDir   string   // root config dir (configDir())
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

// Generate creates Caddyfile blocks for each (extension × port) pair.
// It does NOT write files — callers write the blocks to the appropriate conf.d-<ext>/ dirs.
func Generate(req Request) ([]CaddyBlock, error) {
	info, err := LoadServiceInfo(req.ConfigDir, req.ServiceName)
	if err != nil {
		return nil, err
	}

	displayName := req.DisplayName
	if displayName == "" {
		displayName = req.ServiceName
	}

	// A routes snippet replaces the per-port fan-out: routing happens by path
	// inside the block, so each layer needs exactly one site block. Ports stay
	// optional here — they only supply the number the network layers record.
	if info.Routes != "" {
		// A routes-driven service still declares ports, and a declared
		// subdomain applies to its single site block the same way it would to a
		// generated one — that is how vaultwarden serves vault.<home>.<domain>
		// while keeping its hand-written websocket and rate-limit directives.
		if sub := declaredSubdomain(info.Ports); sub != "" && req.DisplayName == "" {
			displayName = sub
		}
		var blocks []CaddyBlock
		for _, ext := range req.Extensions {
			content, err := buildRoutesBlock(ext, displayName, info.Routes)
			if err != nil {
				return nil, fmt.Errorf("generating block for %s/%s: %w", ext, displayName, err)
			}
			blocks = append(blocks, CaddyBlock{
				Extension: ext,
				Port:      PrimaryPort(info.Ports),
				Content:   content,
			})
		}
		return blocks, nil
	}

	ports, err := ResolvePorts(info.Ports, req.PortNames)
	if err != nil {
		return nil, err
	}

	var blocks []CaddyBlock
	for _, ext := range req.Extensions {
		for _, port := range ports {
			block, err := buildBlock(ext, displayName, req.ServiceName, port)
			if err != nil {
				return nil, fmt.Errorf("generating block for %s/%s/%s: %w", ext, displayName, port.Name, err)
			}
			blocks = append(blocks, block)
		}
	}

	return blocks, nil
}

// declaredSubdomain returns the subdomain a service declares, if it declares
// exactly one. More than one would need more than one site block, which a
// routes body cannot express — those services keep a static caddy.conf.
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

// buildRoutesBlock wraps a layer-agnostic routes body in the site address the
// given layer needs.
func buildRoutesBlock(ext, displayName, routes string) (string, error) {
	var b strings.Builder

	switch ext {
	case "private":
		fmt.Fprintf(&b, "%s {\n", domainForExt(displayName, ext))
		b.WriteString("\timport wildcard_tls\n")
	case "cf":
		// TLS terminated at the Cloudflare edge — serve plain HTTP.
		fmt.Fprintf(&b, "http://%s {\n", domainForExt(displayName, ext))
	// i2p: plain HTTP, and namespaced under the home subdomain — see I2PHost.
	// The transport is already encrypted, and a bare site address would make
	// Caddy chase an ACME cert for a TLD no CA will ever sign.
	case extI2P:
		fmt.Fprintf(&b, "http://%s {\n", I2PHost(displayName, HomeSubdomainVar))
	case "tor", "ygg":
		// Address-addressed, written by their own layers — see buildBlock.
		return "", nil
	default:
		return "", fmt.Errorf("unknown extension: %s", ext)
	}

	b.WriteString(indentBody(stripLeadingComments(routes)))
	b.WriteString("}\n")
	return b.String(), nil
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

// buildBlock generates a single Caddy config block for one extension + port + name combo.
func buildBlock(ext, displayName, svcName string, port PortSelection) (CaddyBlock, error) {
	var content string

	switch ext {
	case "private", "cf":
		// UDP has no site block: Caddy speaks HTTP. The declaration still
		// matters for compose; it just isn't something Caddy can serve.
		if port.RoutableByCaddy() {
			content = buildHTTPBlock(displayName, ext, svcName, port)
		}
	// i2p: Caddy matches the Host header i2pd stamps via hostoverride, which
	// is a name we choose, so the block can be generated here.
	case extI2P:
		// i2pd has exactly one inbound destination per service and delivers to
		// Caddy on :80. A port declared with an explicit listen port (22:22)
		// can never receive traffic there, so it gets no block rather than one
		// that quietly never fires.
		if port.RoutableByCaddy() && port.Listen == 0 {
			content = buildMeshBlock(ext, displayName, svcName, port)
		}
	case "tor", "ygg":
		// Neither address can be templated from a service name, so both
		// layers write their own Caddy config and this returns nothing:
		// yggdrasil routes by an allocated listening port, and a .onion is a
		// hash of a key tor generates. Empty content means "the layer writes
		// it" — callers skip the write.
		content = ""
	default:
		return CaddyBlock{}, fmt.Errorf("unknown extension: %s", ext)
	}

	return CaddyBlock{
		Extension: ext,
		PortName:  port.Name,
		Port:      port.Port,
		Listen:    port.Listen,
		Content:   content,
	}, nil
}

// ── HTTP/TLS block generators ──────────────────────────────────────────────────

func domainForExt(displayName, ext string) string {
	switch ext {
	case "private":
		return fmt.Sprintf("%s.{$HOME_SUBDOMAIN}.{$DOMAIN}", displayName)
	case "cf":
		return fmt.Sprintf("%s.{$DOMAIN}", displayName)
	default:
		return displayName
	}
}

func buildHTTPBlock(displayName, ext, svcName string, port PortSelection) string {
	domain := siteAddress(displayName, ext, port)

	var b strings.Builder
	// CF routes: TLS terminated at Cloudflare edge, serve HTTP-only.
	// Private routes: use wildcard TLS via tailnet.
	if ext == "cf" {
		fmt.Fprintf(&b, "http://%s {\n", domain)
	} else {
		fmt.Fprintf(&b, "%s {\n", domain)
	}
	if ext == "private" {
		b.WriteString("    import wildcard_tls\n")
	}
	fmt.Fprintf(&b, "    reverse_proxy %s:%d\n", svcName, port.Port)

	// WebSocket headers are handled automatically by Caddy v2 reverse_proxy.
	// No need to forward Connection/Upgrade manually.

	b.WriteString("}\n")
	return b.String()
}

// WrapSiteBlock wraps a body of directives in a site block for the given site
// address, stripping the body's file-level comment header and indenting it.
//
// Exported for network layers that have to build their own site address: the
// ygg layer routes by listening port, so only it knows the address until it
// has allocated the port.
func WrapSiteBlock(address, body string) string {
	return address + " {\n" + indentBody(stripLeadingComments(body)) + "}\n"
}

// meshTLD maps a Host-addressed mesh extension to its pseudo-TLD. Yggdrasil is
// absent on purpose — it has no naming, so nothing resolves a ".ygg" host.
var meshTLD = map[string]string{extI2P: "i2p"}

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

// buildMeshBlock generates the site block for one mesh layer + port. Plain
// HTTP only: the transport is already encrypted, and a bare site address
// would make Caddy activate automatic HTTPS and chase an ACME cert for a TLD
// no CA will ever sign.
func buildMeshBlock(ext, displayName, svcName string, port PortSelection) string {
	host := displayName
	if port.Subdomain != "" {
		host = port.Subdomain
	}
	return fmt.Sprintf("http://%s {\n    reverse_proxy %s:%d\n}\n",
		I2PHost(host, HomeSubdomainVar), svcName, port.Port)
}

// siteAddress builds the Caddy site address for one declared port.
//
// A declared subdomain replaces the service name rather than prefixing it, and
// a declared listen port is appended to the address. Both come straight from
// the declaration — see config.PortEntry for the grammar:
//
//	8080      → gitea.home.example.com
//	22:22     → gitea.home.example.com:22
//	vault:80  → vault.home.example.com
func siteAddress(displayName, ext string, port PortSelection) string {
	host := displayName
	if port.Subdomain != "" {
		host = port.Subdomain
	}
	addr := domainForExt(host, ext)
	if port.Listen != 0 {
		addr = fmt.Sprintf("%s:%d", addr, port.Listen)
	}
	return addr
}

// LayerDisplayURL returns the human-readable access URL for a service on the
// given network extension layer. The URL is cosmetic — it matches how Caddy
// routes traffic for this service+layer combination, not the actual network
// address (e.g. Tor .onion addresses are opaque hashes; the displayed URL is
// the Caddy vhost pattern).
//
//   - private: https://<name>.{$HOME_SUBDOMAIN}.{$DOMAIN}
//   - cf:      https://<name>.{$DOMAIN}
//   - tor:     http://<name>.onion (via Caddy)
//   - i2p:     http://<name>.i2p (via Caddy)
//   - ygg:     a placeholder — the real URL is http://[<node address>]:<port>,
//     and neither part is knowable here (see `homelab ygg list`)
//
// ext is one of "private", "cf", "tor", "i2p", "ygg".
// env provides HOME_SUBDOMAIN and DOMAIN for URL template substitution.
// Returns empty string for unknown extensions.
func LayerDisplayURL(ext, displayName string, env map[string]string) string {
	switch ext {
	case "private":
		sub := env["HOME_SUBDOMAIN"]
		dom := env["DOMAIN"]
		if sub != "" && dom != "" {
			return fmt.Sprintf("https://%s.%s.%s", displayName, sub, dom)
		}
		return fmt.Sprintf("https://%s.{HOME_SUBDOMAIN}.{DOMAIN}", displayName)
	case "cf":
		dom := env["DOMAIN"]
		if dom != "" {
			return fmt.Sprintf("https://%s.%s", displayName, dom)
		}
		return fmt.Sprintf("https://%s.{DOMAIN}", displayName)
	case "tor":
		return fmt.Sprintf("http://%s.onion (via Caddy)", displayName)
	case extI2P:
		return fmt.Sprintf("http://%s.i2p (via Caddy)", displayName)
	case "ygg":
		// No naming on the mesh: the URL is the node address and the allocated
		// port, neither of which is knowable from a display name. Callers that
		// can reach the node and socat.d use ygg.ServiceURL instead — which is
		// all of them today; this is the fallback, kept in shape with it.
		// (configgen cannot call it: the ygg layer imports this package.)
		return "http://[<node address: homelab ygg status>]"
	}
	return ""
}

// ExtensionLabel returns a human-readable label for an extension.
func ExtensionLabel(ext string) string {
	switch ext {
	case "cf":
		return "Cloudflare Tunnel"
	case extI2P:
		return "I2P eepsite"
	case "tor":
		return "Tor onion service"
	case "ygg":
		return "Yggdrasil mesh"
	default:
		return ext
	}
}

// ── File writing ───────────────────────────────────────────────────────────────

// ConfigDir returns the extension-specific Caddy config directory.
// Private tailnet configs use "conf.d" (no suffix) for Caddyfile import compat.
func ConfigDir(configRoot, ext string) string {
	if ext == "private" {
		return filepath.Join(configRoot, "caddy", "conf.d")
	}
	return filepath.Join(configRoot, "caddy", "conf.d-"+ext)
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
func GeneratedFilePath(configRoot, ext, svcName, portName string) string {
	return filepath.Join(ConfigDir(configRoot, ext), PortFileName(svcName, portName)+".conf")
}

// WriteFile writes a generated Caddy config block to the extension-specific conf.d directory.
func WriteFile(configRoot, ext, svcName, portName, content string) error {
	dir := ConfigDir(configRoot, ext)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	return os.WriteFile(GeneratedFilePath(configRoot, ext, svcName, portName), []byte(content), 0o600)
}

// RemoveFile removes a generated Caddy config file for the given service/extension/port.
func RemoveFile(configRoot, ext, svcName, portName string) error {
	err := os.Remove(GeneratedFilePath(configRoot, ext, svcName, portName))
	if os.IsNotExist(err) {
		return nil // already removed
	}
	return err
}

// RemoveAllPortFiles removes the generated Caddy config for every port the
// service declares under the given extension. Since a multi-port service
// gets one generated file per port (see PortFileName), removing only the
// default-named file (portName "") would orphan the rest. Falls back to a
// single default-name removal for services with no declared ports (legacy
// static-caddy.conf services, or a config.yaml with none at all) — that's
// the only file that could exist for them.
func RemoveAllPortFiles(configRoot, ext, svcName string) error {
	info, err := LoadServiceInfo(configRoot, svcName)
	// A routes-driven service has exactly one file per layer regardless of how
	// many ports it declares, so per-port removal would miss it.
	if err == nil && info.Routes != "" {
		return RemoveFile(configRoot, ext, svcName, "")
	}
	if err != nil || len(info.Ports) == 0 {
		return RemoveFile(configRoot, ext, svcName, "")
	}
	ports, err := ResolvePorts(info.Ports, nil)
	if err != nil {
		return RemoveFile(configRoot, ext, svcName, "")
	}
	var firstErr error
	for _, p := range ports {
		if err := RemoveFile(configRoot, ext, svcName, p.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
