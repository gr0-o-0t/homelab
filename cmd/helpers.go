package cmd

import (
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/tui/styles"
)

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
