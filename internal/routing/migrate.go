package routing

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
)

// Migration from the per-layer layout.
//
// Before exposure.yaml, a service's exposure was the set of files it had in
// caddy/conf.d, conf.d-cf, conf.d-tor, conf.d-i2p and conf.d-ygg (one per
// layer and port, named by configgen.PortFileName). Migrate infers the state
// those files imply, writes exposure.yaml, renders caddy/sites/<svc>.conf and
// only then removes the legacy files — so an exposure is never lost, and a
// run that fails part-way resumes from the exposure.yaml it already wrote.
// Daemon-side files (torrc.d, tunnels.conf, socat.d) are read, never written.

// Migrated describes one service's migration.
type Migrated struct {
	Service string
	State   exposure.State
	Removed []string // legacy files removed
}

// legacyFiles returns the service's legacy Caddy files that exist, by layer
// name. Names are computed from the declared ports, not globbed: "foo-*" would
// also match a service called "foo-bar".
func legacyFiles(root string, reg *network.Registry, svc string, info configgen.ServiceInfo) map[string][]string {
	names := []string{svc}
	for p := range info.Ports {
		if n := configgen.PortFileName(svc, p); !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	out := map[string][]string{}
	for _, l := range reg.All() {
		for _, n := range names {
			path := filepath.Join(root, "caddy", l.LegacyConfDir(), n+".conf")
			if _, err := os.Stat(path); err == nil {
				out[l.Name()] = append(out[l.Name()], path)
			}
		}
	}
	return out
}

// HasLegacy reports whether a service still has legacy per-layer Caddy files.
func HasLegacy(root string, reg *network.Registry, svc string) bool {
	info, _ := configgen.LoadServiceInfo(root, svc)
	info.Name = svc
	return len(legacyFiles(root, reg, svc, info)) > 0
}

// Pending lists the installed services that still have legacy per-layer
// files. Cheap when there are none: one directory read per layer.
func Pending(root string, reg *network.Registry) ([]string, error) {
	present := map[string]bool{}
	for _, l := range reg.All() {
		des, err := os.ReadDir(filepath.Join(root, "caddy", l.LegacyConfDir()))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, de := range des {
			if base, ok := strings.CutSuffix(de.Name(), ".conf"); ok {
				present[base] = true
			}
		}
	}
	if len(present) == 0 {
		return nil, nil
	}
	des, err := os.ReadDir(filepath.Join(root, "services"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []string
	for _, de := range des {
		svc := de.Name()
		if !de.IsDir() || !mayHaveLegacy(present, svc) {
			continue
		}
		if HasLegacy(root, reg, svc) {
			out = append(out, svc)
		}
	}
	return out, nil
}

func mayHaveLegacy(present map[string]bool, svc string) bool {
	for base := range present {
		if base == svc || strings.HasPrefix(base, svc+"-") {
			return true
		}
	}
	return false
}

// Migrate migrates every pending service. A service that fails is left as it
// was (still routed by its legacy files) and reported in the joined error;
// the others are migrated regardless.
func Migrate(root string, reg *network.Registry) ([]Migrated, error) {
	pending, err := Pending(root, reg)
	if err != nil {
		return nil, err
	}
	var done []Migrated
	var errs []error
	for _, svc := range pending {
		m, err := MigrateService(root, reg, svc)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", svc, err))
			continue
		}
		done = append(done, m)
	}
	return done, errors.Join(errs...)
}

// MigrateService migrates one service. With no legacy files it does nothing.
func MigrateService(root string, reg *network.Registry, svc string) (Migrated, error) {
	info, err := configgen.LoadServiceInfo(root, svc)
	if err != nil {
		return Migrated{}, err
	}
	info.Name = svc
	legacy := legacyFiles(root, reg, svc, info)
	if len(legacy) == 0 {
		return Migrated{Service: svc}, nil
	}

	// An exposure.yaml next to legacy files is a migration that got as far as
	// writing it: resume from it rather than inferring again.
	if _, err := os.Stat(exposure.Path(root, svc)); errors.Is(err, os.ErrNotExist) {
		st, err := inferState(root, reg, svc, info, legacy)
		if err != nil {
			return Migrated{}, err
		}
		if err := exposure.Save(root, svc, st); err != nil {
			return Migrated{}, err
		}
	}
	st, err := exposure.Load(root, svc)
	if err != nil {
		return Migrated{}, err
	}

	// The onion is in the legacy block; tor's key directory may not be
	// readable from here, and the daemon may be down.
	if paths := legacy["tor"]; len(paths) > 0 {
		if l, ok := reg.Get("tor"); ok {
			if r, ok := l.(interface{ RememberOnion(svc, onion string) }); ok {
				if onion := onionFromBlock(paths[0]); onion != "" {
					r.RememberOnion(svc, onion)
				}
			}
		}
	}
	if err := Sync(root, reg, svc); err != nil {
		return Migrated{}, fmt.Errorf("rendering %s: %w", configgen.SitesFile(root, svc), err)
	}

	m := Migrated{Service: svc, State: st}
	for _, l := range reg.All() {
		for _, path := range legacy[l.Name()] {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return m, err
			}
			m.Removed = append(m.Removed, path)
		}
	}
	return m, nil
}

// inferState reconstructs a service's exposure from its legacy files.
//
//   - layers: those with any of its files in their conf dir;
//   - name: the host label in a host-routed layer's block, when it differs
//     from the one the service would get without --name;
//   - ports: the declared ports that have a file in the layers that would
//     have written one for them (ts/cf/i2p only route HTTP ports without a
//     listen port; ygg routes every port). A port no enabled layer could show
//     is assumed selected, since no --ports is the common case. All selected
//     is stored as no selection. Layers disagreeing about a port is an error
//     (except ygg lacking it), since one selection now covers them all;
//   - ygg_ports: PORT= from each selected port's socat.d forwarder, else the
//     `:<port>` address of its legacy ygg block; a port with neither is not on
//     the mesh.
func inferState(root string, reg *network.Registry, svc string, info configgen.ServiceInfo, legacy map[string][]string) (exposure.State, error) {
	var st exposure.State
	for _, l := range reg.All() {
		if len(legacy[l.Name()]) > 0 {
			st.Set(l.Name(), true)
		}
	}
	has := func(layer, file string) bool {
		return slices.Contains(legacy[layer], filepath.Join(root, "caddy", legacyDir(reg, layer), file+".conf"))
	}
	hostLayers := []string{"ts", "cf", "i2p"}

	var ports []network.PortSelection
	if info.Routes != "" {
		ports = []network.PortSelection{{}}
		st.Name = inferName(reg, legacy, hostLayers, svc, configgen.SiteHost(info, ""), func(string) string { return svc })
	} else {
		all, err := configgen.ResolvePorts(info.Ports, nil)
		if err != nil {
			return st, err
		}
		var selected []string
		for _, p := range all {
			httpSite := p.RoutableByCaddy() && p.Listen == 0
			var saw, missed []string
			for _, layer := range append(hostLayers, "ygg") {
				if !st.On(layer) || (layer != "ygg" && !httpSite) {
					continue
				}
				if has(layer, configgen.PortFileName(svc, p.Name)) {
					saw = append(saw, layer)
				} else {
					missed = append(missed, layer)
				}
			}
			// One selection now covers every layer, so layers that disagree
			// about a port cannot be migrated without exposing more or less
			// than before — except ygg lacking one, which its mesh ports
			// express (no allocation, no block).
			if len(saw) > 0 && len(missed) > 0 && !slices.Equal(missed, []string{"ygg"}) {
				return st, fmt.Errorf("port %q is routed on %s but not on %s, and a service now has one --ports "+
					"selection for every layer; make the layers agree (remove the extra legacy file, or "+
					"enable the port on the others) and migrate again",
					p.Name, strings.Join(saw, ", "), strings.Join(missed, ", "))
			}
			if len(saw) > 0 || len(missed) == 0 {
				selected = append(selected, p.Name)
				ports = append(ports, p)
			}
		}
		if len(selected) < len(all) && len(selected) > 0 {
			st.Ports = selected
		} else {
			ports = all
		}
		// The name labels the ports without a declared subdomain.
		var plain string
		for _, p := range ports {
			if p.RoutableByCaddy() && p.Listen == 0 && p.Subdomain == "" {
				plain = configgen.PortFileName(svc, p.Name)
				break
			}
		}
		if plain != "" {
			st.Name = inferName(reg, legacy, hostLayers, svc, svc, func(string) string { return plain })
		}
	}

	if st.On("ygg") {
		st.YggPorts = map[string]int{}
		for _, p := range ports {
			file := configgen.PortFileName(svc, p.Name)
			port := forwardPort(filepath.Join(root, "yggdrasil", "socat.d", file+".forward"))
			if port == 0 {
				port = meshPortFromBlock(filepath.Join(root, "caddy", legacyDir(reg, "ygg"), file+".conf"))
			}
			if port != 0 {
				st.YggPorts[exposure.MeshKey(p.Name)] = port
			}
		}
		if len(st.YggPorts) == 0 {
			return st, errors.New("on ygg, but no mesh port found in socat.d or its legacy blocks")
		}
	}
	return st, nil
}

// inferName reads the host label out of the first host-routed layer's block
// for file and returns it when it differs from def — that is, when the
// service was enabled with --name.
func inferName(reg *network.Registry, legacy map[string][]string, hostLayers []string, svc, def string, file func(string) string) string {
	for _, layer := range hostLayers {
		l, ok := reg.Get(layer)
		if !ok {
			continue
		}
		hr, ok := l.(network.HostRouted)
		if !ok {
			continue
		}
		for _, path := range legacy[layer] {
			if filepath.Base(path) != file(svc)+".conf" {
				continue
			}
			if label, ok := hr.HostFromAddress(siteAddress(path)); ok {
				if label == def {
					return ""
				}
				return label
			}
		}
	}
	return ""
}

func legacyDir(reg *network.Registry, layer string) string {
	if l, ok := reg.Get(layer); ok {
		return l.LegacyConfDir()
	}
	return ""
}

// siteAddress returns the address of the first site block in a Caddy file.
func siteAddress(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // legacy file under the config dir
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return strings.TrimSpace(strings.TrimSuffix(line, "{"))
	}
	return ""
}

// onionFromBlock extracts <x>.onion from a legacy `http://<x>.onion:8081` site.
func onionFromBlock(path string) string {
	addr := strings.TrimPrefix(siteAddress(path), "http://")
	host, _, _ := strings.Cut(addr, ":")
	if !strings.HasSuffix(host, ".onion") {
		return ""
	}
	return host
}

// meshPortFromBlock extracts the port from a legacy `:<port>` ygg site.
func meshPortFromBlock(path string) int {
	rest, ok := strings.CutPrefix(siteAddress(path), ":")
	if !ok {
		return 0
	}
	p, _ := strconv.Atoi(rest)
	return p
}

// forwardPort reads PORT=<n> from a socat.d forwarder; 0 if absent.
func forwardPort(path string) int {
	data, err := os.ReadFile(path) //nolint:gosec // daemon config under the config dir
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "PORT="); ok {
			if p, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				return p
			}
		}
	}
	return 0
}
