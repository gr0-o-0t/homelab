package cmd

import (
	"fmt"
	"strconv"

	"github.com/groot/homelab/internal/configgen"

	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/tui/styles"
)

// detectServicePort returns the port a layer should proxy a service to.
//
// The declared `ports:` in the service's config.yaml is the source of truth —
// the same one `homelab enable` uses, so `tor enable` and `enable --tor` can
// no longer pick different ports for the same service.
func detectServicePort(root, name string) (string, error) {
	info, err := configgen.LoadServiceInfo(root, name)
	if err == nil && len(info.Ports) > 0 {
		return strconv.Itoa(configgen.PrimaryPort(info.Ports)), nil
	}
	return "", fmt.Errorf("no ports declared in config.yaml for %s", name)
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
