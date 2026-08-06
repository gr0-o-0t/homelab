package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/groot/homelab/internal/configgen"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/tui/styles"
)

// detectServicePort returns the port a layer should proxy a service to.
//
// The declared `ports:` in the service's config.yaml is the source of truth —
// the same one `homelab enable` uses, so `tor enable` and `enable --tor` can
// no longer pick different ports for the same service. Scraping caddy.conf,
// which is what this used to do exclusively, reads a *generated* file to
// recover an input, and finds nothing at all for the routes-driven services
// (appflowy, immich) whose caddy.routes.conf has no `reverse_proxy <name>:`
// line for the default port.
//
// The scrape survives as a fallback for legacy services that ship a static
// caddy.conf and declare no ports.
func detectServicePort(root, name string) (string, error) {
	info, err := configgen.LoadServiceInfo(root, name)
	if err == nil && len(info.Ports) > 0 {
		return strconv.Itoa(configgen.PrimaryPort(info.Ports)), nil
	}
	port, scrapeErr := portFromCaddyConf(root, name)
	if scrapeErr != nil {
		return "", fmt.Errorf("no ports declared in config.yaml and %w", scrapeErr)
	}
	return port, nil
}

// portFromCaddyConf recovers the upstream port from a legacy static caddy.conf.
func portFromCaddyConf(root, name string) (string, error) {
	caddyConf := filepath.Join(root, "services", name, "caddy.conf")
	data, err := os.ReadFile(caddyConf) // nosec G304 -- path is programmatically constructed
	if err != nil {
		return "", err
	}
	line := string(data)
	prefix := "reverse_proxy " + name + ":"
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return "", fmt.Errorf("could not find 'reverse_proxy %s:<port>' in caddy.conf", name)
	}
	rest := line[idx+len(prefix):]
	end := strings.IndexAny(rest, " \t\n\r")
	if end < 0 {
		return "", fmt.Errorf("malformed reverse_proxy line")
	}
	return rest[:end], nil
}

// addressText renders one resolved layer address for humans: the URL, plus its
// qualifier when it has one. An address with no URL is all qualifier — "not
// generated yet", "node not running" — which is still worth showing, because
// the alternative is the invented address this used to print.
func addressText(a network.ServiceAddress) string {
	switch {
	case a.URL == "":
		return styles.Muted.Render("(" + a.Note + ")")
	case a.Note == "":
		return a.URL
	default:
		return a.URL + " " + styles.Muted.Render("("+a.Note+")")
	}
}

// layerTag renders a layer's short name in its display colour.
func layerTag(name string) string {
	switch name {
	case "ts":
		return styles.Success.Render("ts")
	case "cf":
		return styles.Primary.Render("cf")
	case "tor":
		return styles.Accent.Render("tor")
	case "i2p":
		return styles.Warning.Render("i2p")
	case "ygg":
		return styles.Primary.Render("ygg")
	default:
		return styles.Muted.Render(name)
	}
}
