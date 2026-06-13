// Package configgen generates Caddy config blocks from service port declarations
// and extension flags. Used by `homelab enable <service> --cf --i2p ...`.
package configgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/config"
)

// PortSelection is one resolved port to expose.
type PortSelection struct {
	Name     string // "web", "ssh", or "default"
	Port     int    // container port number
	Protocol string // "tcp" or "udp"
}

// CaddyBlock is one generated Caddy config block.
type CaddyBlock struct {
	Extension string // "private", "cf", "i2p", "tor", "ygg"
	PortName  string // port name
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

// ServiceInfo holds the parsed service configuration needed for generation.
type ServiceInfo struct {
	Name    string
	Ports   map[string]config.PortEntry
	HasVars bool // whether config.yaml was found and parsed
}

// LoadServiceInfo reads a service's config.yaml and returns its port info.
// Returns an empty ServiceInfo (no error) if config.yaml doesn't exist.
func LoadServiceInfo(configDir, svcName string) (ServiceInfo, error) {
	info := ServiceInfo{Name: svcName, Ports: make(map[string]config.PortEntry)}

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
func ResolvePorts(ports map[string]config.PortEntry, selected []string) ([]PortSelection, error) {
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
		proto := strings.ToLower(entry.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		result = append(result, PortSelection{
			Name:     name,
			Port:     entry.Port,
			Protocol: proto,
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

// buildBlock generates a single Caddy config block for one extension + port + name combo.
func buildBlock(ext, displayName, svcName string, port PortSelection) (CaddyBlock, error) {
	var content string

	switch ext {
	case "private", "cf":
		content = buildHTTPBlock(displayName, ext, svcName, port)
	case "i2p":
		// i2p: Caddy matches on Host header from i2pd's hostoverride
		content = buildI2PBlock(displayName, svcName, port)
	case "tor":
		content = buildTorBlock(displayName, svcName, port)
	case "ygg":
		content = buildYggBlock(displayName, svcName, port)
	default:
		return CaddyBlock{}, fmt.Errorf("unknown extension: %s", ext)
	}

	return CaddyBlock{
		Extension: ext,
		PortName:  port.Name,
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
	domain := domainForExt(displayName, ext)
	sub := portSubdomain(displayName, port.Name)
	if sub != "" {
		domain = sub + "." + domain
	}

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

func buildI2PBlock(displayName, svcName string, port PortSelection) string {
	sub := portSubdomain(displayName, port.Name)
	host := displayName
	if sub != "" {
		host = sub + "." + host
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s.i2p {\n", host)
	fmt.Fprintf(&b, "    reverse_proxy %s:%d\n", svcName, port.Port)
	b.WriteString("}\n")
	return b.String()
}

func buildTorBlock(displayName, svcName string, port PortSelection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s.onion {\n", displayName)
	fmt.Fprintf(&b, "    reverse_proxy %s:%d\n", svcName, port.Port)
	b.WriteString("}\n")
	return b.String()
}

func buildYggBlock(displayName, svcName string, port PortSelection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Yggdrasil route: %s → %s:%d\n", displayName, svcName, port.Port)
	fmt.Fprintf(&b, "%s.ygg {\n", displayName)
	fmt.Fprintf(&b, "    reverse_proxy %s:%d\n", svcName, port.Port)
	b.WriteString("}\n")
	return b.String()
}

// portSubdomain returns a subdomain prefix when the port name is non-default.
// Only the unnamed/default port ("default" or the only HTTP port) gets no prefix.
func portSubdomain(displayName, portName string) string {
	// If there's only one port named "web" or "default", no subdomain needed
	if portName == "default" || portName == "web" {
		return ""
	}
	// Named port → subdomain prefix
	return portName
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
//   - ygg:     http://<name>.ygg
//   - ipfs:    ipfs://<name>
//
// ext is one of "private", "cf", "tor", "i2p", "ygg", "ipfs".
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
	case "i2p":
		return fmt.Sprintf("http://%s.i2p (via Caddy)", displayName)
	case "ygg":
		return fmt.Sprintf("http://%s.ygg", displayName)
	case "ipfs":
		return fmt.Sprintf("ipfs://%s", displayName)
	}
	return ""
}

// ExtensionLabel returns a human-readable label for an extension.
func ExtensionLabel(ext string) string {
	switch ext {
	case "cf":
		return "Cloudflare Tunnel"
	case "i2p":
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

// WriteFile writes a generated Caddy config block to the extension-specific conf.d directory.
func WriteFile(configRoot, ext, svcName, portName, content string) error {
	dir := ConfigDir(configRoot, ext)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	// File name pattern: <service>-<port>.conf or <service>.conf for default port
	filename := svcName
	if portName != "" && portName != "default" && portName != "web" {
		filename = svcName + "-" + portName
	}
	// For private/cf, don't add port suffix to keep backward compat
	if ext == "private" || ext == "cf" {
		filename = svcName
	}

	path := filepath.Join(dir, filename+".conf")
	return os.WriteFile(path, []byte(content), 0o600)
}

// RemoveFile removes a generated Caddy config file for the given service/extension/port.
func RemoveFile(configRoot, ext, svcName, portName string) error {
	dir := ConfigDir(configRoot, ext)
	filename := svcName
	if portName != "" && portName != "default" && portName != "web" {
		filename = svcName + "-" + portName
	}
	if ext == "private" || ext == "cf" {
		filename = svcName
	}
	path := filepath.Join(dir, filename+".conf")
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil // already removed
	}
	return err
}
