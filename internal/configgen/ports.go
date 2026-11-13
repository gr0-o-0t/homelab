package configgen

import (
	"fmt"
	"slices"
	"strings"

	"github.com/groot/homelab/internal/exposure"
)

// PortStatus is one declared port of a service as `homelab status` reports
// it, in `status --json` and `status <svc> --json` as the service's "ports":
//
//	{"name": "2222", "label": "2222/tcp", "raw": true, "port": 2222, "listen": 2222,
//	 "protocols": ["tcp"], "target": "gitea.home.example.com:2222/tcp", "enabled": true}
//	{"name": "default", "label": "https", "port": 3000,
//	 "target": "https://gitea.home.example.com", "enabled": true}
type PortStatus struct {
	// Name is the port as `homelab enable --ports=` names it: "default" for
	// a bare port, the subdomain for name:port, the listen port for
	// listen:port. Empty for a caddy.routes.conf service, whose single site
	// --ports does not select.
	Name string `json:"name"`
	// Label is the PORTS column: "https" for the default site, the
	// subdomain ("vault"), or "<listen>/<proto>" for a raw port ("2222/tcp").
	Label string `json:"label"`
	// Raw: a listen port, proxied byte for byte by caddy-l4 on the tailnet
	// node, not an HTTP site.
	Raw bool `json:"raw,omitempty"`
	// Port is the container port traffic is forwarded to.
	Port int `json:"port"`
	// Listen is a raw port's listen port on the tailnet node.
	Listen int `json:"listen,omitempty"`
	// Protocols are a raw (or unroutable udp) port's protocols.
	Protocols []string `json:"protocols,omitempty"`
	// Target is where the port answers (or would, once enabled): a URL for
	// an HTTP port, "<host>:<listen>/<proto>" for a raw one. Empty when
	// HOME_SUBDOMAIN/DOMAIN are not set, or the port is never routed.
	Target string `json:"target,omitempty"`
	// Enabled: exposed now — the service is on a layer that serves this
	// kind of port and exposure.yaml's ports: selection includes it.
	Enabled bool `json:"enabled"`
	// Note says why a port is never routed, when it is not.
	Note string `json:"note,omitempty"`
}

// PortStatuses reports every port a service declares in its config.yaml,
// with what its exposure.yaml enables. nil when it declares none (or its
// config cannot be read).
func PortStatuses(root, svc string, env map[string]string) []PortStatus {
	info, err := LoadServiceInfo(root, svc)
	if err != nil || len(info.Ports) == 0 {
		return nil
	}
	st, _ := exposure.Load(root, svc)
	onTS, onCF := slices.Contains(st.Layers, "ts"), slices.Contains(st.Layers, "cf")
	host := st.Name
	if host == "" {
		host = svc
	}
	sub, dom := env["HOME_SUBDOMAIN"], env["DOMAIN"]
	// url is an HTTP site's address: the tailnet's, or Cloudflare's when the
	// service is public only.
	url := func(label string) string {
		switch {
		case dom == "":
			return ""
		case onCF && !onTS:
			return fmt.Sprintf("https://%s.%s", label, dom)
		case sub == "":
			return ""
		}
		return fmt.Sprintf("https://%s.%s.%s", label, sub, dom)
	}

	if info.Routes != "" {
		// One site at the service's host; --ports does not apply.
		label := SiteHost(info, st.Name)
		p := PortStatus{Label: label, Port: PrimaryPort(info.Ports), Target: url(label), Enabled: len(st.Layers) > 0}
		if label == svc {
			p.Label = "https"
		}
		return []PortStatus{p}
	}

	resolved, err := ResolvePorts(info.Ports, nil)
	if err != nil {
		return nil
	}
	selected := func(name string) bool { return len(st.Ports) == 0 || slices.Contains(st.Ports, name) }
	out := make([]PortStatus, 0, len(resolved))
	for _, r := range resolved {
		p := PortStatus{Name: r.Name, Port: r.Port}
		switch {
		case r.Listen != 0:
			protos := strings.Join(r.Protocols, "+")
			p.Raw, p.Listen, p.Protocols = true, r.Listen, r.Protocols
			p.Label = fmt.Sprintf("%d/%s", r.Listen, protos)
			if sub != "" && dom != "" {
				p.Target = fmt.Sprintf("%s.%s.%s:%d/%s", host, sub, dom, r.Listen, protos)
			}
			p.Enabled = onTS && selected(r.Name)
		case !r.RoutableByCaddy():
			p.Protocols = r.Protocols
			p.Label = fmt.Sprintf("%d/%s", r.Port, strings.Join(r.Protocols, "+"))
			p.Note = "never routed: a udp port needs a listen port (53:53/udp)"
		default:
			label := host
			p.Label = "https"
			if r.Subdomain != "" {
				label, p.Label = r.Subdomain, r.Subdomain
			}
			p.Target = url(label)
			p.Enabled = len(st.Layers) > 0 && selected(r.Name)
		}
		out = append(out, p)
	}
	return out
}

// EnabledPortLabels is the PORTS column: the labels of the enabled ports.
func EnabledPortLabels(ports []PortStatus) []string {
	var out []string
	for _, p := range ports {
		if p.Enabled {
			out = append(out, p.Label)
		}
	}
	return out
}
