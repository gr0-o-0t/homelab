// Package tor implements the NetworkLayer interface for Tor onion service proxy.
// Manages tor container lifecycle and per-service .onion hidden service configs
// via torrc.d/ configuration files and SIGHUP reloads.
package tor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/run"
)

const (
	containerName       = "tor"
	torHiddenServiceDir = "/var/lib/tor/hidden_service"

	// caddyUpstream is how anything outside Caddy's network namespace reaches
	// it: Caddy runs as network_mode: service:tailscale, so Docker DNS has no
	// "caddy" record. cloudflared and i2pd use the same address.
	caddyUpstream = "tailscale"

	onionWaitAttempts = 20
	onionWaitInterval = 500 * time.Millisecond
)

// Layer implements network.NetworkLayer for Tor onion services.
type Layer struct {
	repoRoot string
	runner   *run.Commander
	envFn    network.EnvFunc

	// reloadHook, if non-nil, replaces reload() for testing.
	reloadHook func() error

	// onionCache memoizes per-service .onion lookups; see network.AddressCache.
	cacheMu    sync.Mutex
	onionCache map[string]*network.AddressCache

	// onionHook, if non-nil, replaces the container read in OnionAddress.
	onionHook func(svcName string) string
}

// New creates a new Tor layer.
func New(repoRoot string, runner *run.Commander, envFn network.EnvFunc) *Layer {
	return &Layer{repoRoot: repoRoot, runner: runner, envFn: envFn}
}

// newForTest creates a Tor layer with an injected reload hook for testing.
func newForTest(repoRoot string, hook func() error) *Layer {
	return &Layer{
		repoRoot: repoRoot, runner: run.Default(), reloadHook: hook,
		// Tests have no tor container; pretend the address exists so Enable
		// exercises the Caddy-block path instead of polling for 10s.
		onionHook: func(svcName string) string { return svcName + "onionaddressxxxxxxxxxxxx.onion" },
	}
}

// compile-time check
var _ network.NetworkLayer = (*Layer)(nil)

// ── Identity ──────────────────────────────────────────────────────────────────

func (l *Layer) Name() string          { return "tor" }
func (l *Layer) Label() string         { return "Tor onion service proxy" }
func (l *Layer) ContainerName() string { return containerName }
func (l *Layer) Profile() string       { return "tor" }

// ── Lifecycle ─────────────────────────────────────────────────────────────────

func (l *Layer) Start() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot),
		l.env(),
		"--profile", "tor", "up", "-d",
	)
}

func (l *Layer) Stop() error {
	return l.runner.DockerComposeEnv(
		run.CoreComposeFile(l.repoRoot),
		l.env(),
		"stop", containerName,
	)
}

func (l *Layer) Status() network.Status {
	state := l.runner.ContainerStatus(containerName)
	return network.Status{ContainerState: state}
}

// ── Service exposure ─────────────────────────────────────────────────────────

// Enable writes torrc.d config for the service and reloads tor. Caddy config
// (routing <displayName>.onion → svcName:<port>) is written separately by
// internal/configgen — see cmd/enable.go.
func (l *Layer) Enable(svcName, displayName string, info network.ServiceInfo, ports []network.PortSelection) error {
	if err := l.writeTorService(svcName, ports); err != nil {
		return fmt.Errorf("writing torrc config: %w", err)
	}
	if err := l.reload(); err != nil {
		return err
	}
	return l.writeCaddyBlock(svcName, ports)
}

// Disable removes torrc.d config for the service and reloads. Caddy config
// removal is handled separately by internal/configgen.
func (l *Layer) Disable(svcName string) error {
	if err := os.Remove(l.torServicePath(svcName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing torrc config: %w", err)
	}
	// The Caddy block is this layer's too, now that it carries the generated
	// .onion — leaving it behind would keep a site block for an address no
	// hidden service answers on.
	block := filepath.Join(l.CaddyConfigDir(l.repoRoot), svcName+".conf")
	if err := os.Remove(block); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing caddy config: %w", err)
	}
	return l.reload()
}

// ── Config ────────────────────────────────────────────────────────────────────

func (l *Layer) CaddyConfigDir(configRoot string) string {
	return filepath.Join(configRoot, "caddy", "conf.d-tor")
}

// ServiceAddresses returns the service's .onion, read from the hostname file
// Tor generates when it builds the hidden service.
//
// There is nothing to template here: an onion address is a hash of the
// service's key, so a name-derived "<svc>.onion" — which is what callers used
// to print — is always fiction.
func (l *Layer) ServiceAddresses(svcName string, _ map[string]string) []network.ServiceAddress {
	onion := l.OnionAddress(svcName)
	if onion == "" {
		return []network.ServiceAddress{{
			Note: "address not generated yet — start tor and re-check",
		}}
	}
	return []network.ServiceAddress{{URL: "http://" + onion}}
}

// OnionAddress returns the generated .onion for a service, or "" when tor
// isn't running or hasn't created the hidden service yet.
func (l *Layer) OnionAddress(svcName string) string {
	if l.onionHook != nil {
		return l.onionHook(svcName)
	}
	return l.cacheFor(svcName).Get(func() string {
		if l.runner == nil {
			return ""
		}
		out, err := l.runner.Output("docker", "exec", containerName,
			"cat", torHiddenServiceDir+"/"+svcName+"/hostname")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	})
}

// cacheFor returns this service's address cache, creating it on first use.
// Per service, because every hidden service has its own .onion.
func (l *Layer) cacheFor(svcName string) *network.AddressCache {
	l.cacheMu.Lock()
	defer l.cacheMu.Unlock()
	if l.onionCache == nil {
		l.onionCache = map[string]*network.AddressCache{}
	}
	if l.onionCache[svcName] == nil {
		l.onionCache[svcName] = &network.AddressCache{}
	}
	return l.onionCache[svcName]
}

// ── Tor-specific helpers ─────────────────────────────────────────────────────

// checkHiddenServiceDir verifies tor will be able to create its per-service
// key directory, and explains the fix when it won't.
//
// The failure is always the same one: the bind-mount target did not exist when
// the container first started, so Docker created it as root:root. Reporting a
// bare "permission denied" from a mkdir deep inside enable told nobody what to
// do about it.
func (l *Layer) checkHiddenServiceDir() error {
	dir := filepath.Join(l.repoRoot, "tor", "hidden_service")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	probe := filepath.Join(dir, ".homelab-write-probe")
	if err := os.Mkdir(probe, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf(
			"%s is not writable, so tor cannot create its key directory there.\n"+
				"  Docker created it as root because it did not exist when the container first started.\n"+
				"  Fix with: sudo chown -R \"$(id -u):$(id -g)\" %s",
			dir, dir)
	}
	return os.Remove(probe)
}

func (l *Layer) torServicePath(name string) string {
	return filepath.Join(l.repoRoot, "tor", "torrc.d", name+".conf")
}

// writeTorService writes the service's torrc.d snippet.
//
// HTTP ports are pointed at Caddy (tailscale:80), not at the service
// container. Going direct — which this used to do — meant onion traffic never
// reached Caddy at all, so the conf.d-tor site blocks were decoration and any
// service whose routing is more than one upstream (a caddy.routes.conf path
// fan-out) was simply broken over Tor.
//
// Ports with an explicit listen port keep going direct, and must: an ssh port
// declared 22:22 is not HTTP, and putting Caddy in that path would break it.
func (l *Layer) writeTorService(name string, ports []network.PortSelection) error {
	confDir := filepath.Join(l.repoRoot, "tor", "torrc.d")
	if err := os.MkdirAll(confDir, 0o750); err != nil {
		return fmt.Errorf("creating torrc.d: %w", err)
	}
	// The per-service key directory is tor's to create, not ours: tor makes it
	// mode 0700 and refuses to start if it finds one more permissive, so
	// pre-creating it here (this used to, at 0777) either fails or breaks the
	// daemon. What we do check is that tor can create it — the bind-mount
	// parent has to be writable by the uid tor runs as, which is this uid.
	if err := l.checkHiddenServiceDir(); err != nil {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "HiddenServiceDir %s/%s\n", torHiddenServiceDir, name)
	httpRouted := false
	for _, p := range ports {
		if p.Listen != 0 {
			fmt.Fprintf(&b, "HiddenServicePort %d %s:%d\n", p.Listen, name, p.Port)
			continue
		}
		if httpRouted {
			continue // one :80 vhost per onion; Caddy splits by Host from there
		}
		fmt.Fprintf(&b, "HiddenServicePort 80 %s:80\n", caddyUpstream)
		httpRouted = true
	}
	return os.WriteFile(l.torServicePath(name), []byte(b.String()), 0o600)
}

// writeCaddyBlock writes the site block for this service's onion address.
//
// Like the ygg layer, tor owns its Caddy config: the address is a hash of a
// key tor generates, so it cannot be templated from a service name and is only
// known after tor has loaded the config. Nothing rewrites the Host header on
// the way in — unlike i2pd's hostoverride — so the site address has to be the
// real .onion.
func (l *Layer) writeCaddyBlock(svcName string, ports []network.PortSelection) error {
	onion := l.waitForOnion(svcName)
	if onion == "" {
		return fmt.Errorf(
			"tor has not published an address for %s yet.\n"+
				"  The hidden service is configured; re-run this command once "+
				"`homelab tor list` shows its .onion to finish the Caddy route",
			svcName)
	}

	body, err := l.routeBody(svcName, ports)
	if err != nil {
		return err
	}
	dir := l.CaddyConfigDir(l.repoRoot)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating caddy config dir: %w", err)
	}
	block := "# Tor: " + svcName + "\n" +
		configgen.WrapSiteBlock("http://"+onion, body)
	return os.WriteFile(filepath.Join(dir, svcName+".conf"), []byte(block), 0o600)
}

// routeBody is the service's routes file when it has one, else a plain proxy
// to its HTTP port — the same body every other layer serves.
func (l *Layer) routeBody(svcName string, ports []network.PortSelection) (string, error) {
	info, err := configgen.LoadServiceInfo(l.repoRoot, svcName)
	if err != nil {
		return "", fmt.Errorf("reading service config: %w", err)
	}
	if info.Routes != "" {
		return info.Routes, nil
	}
	for _, p := range ports {
		if p.Listen == 0 {
			return fmt.Sprintf("reverse_proxy %s:%d\n", svcName, p.Port), nil
		}
	}
	return "", fmt.Errorf("%s declares no HTTP port to route over tor", svcName)
}

// waitForOnion polls for the address tor generates when it loads the config.
// Tor writes the hostname file as soon as it creates the key, so this is a
// short wait, not a wait for the descriptor to publish.
func (l *Layer) waitForOnion(svcName string) string {
	// No point polling a container that isn't up: the caller's error message
	// already says to start tor and re-run.
	if l.onionHook == nil && l.runner != nil &&
		l.runner.ContainerStatus(containerName) != "running" {
		return ""
	}
	for range onionWaitAttempts {
		if addr := l.OnionAddress(svcName); addr != "" {
			return addr
		}
		time.Sleep(onionWaitInterval)
	}
	return ""
}

func (l *Layer) reload() error {
	if l.reloadHook != nil {
		return l.reloadHook()
	}
	return l.runner.DockerExec(containerName,
		"sh", "-c", "kill -HUP $(pidof tor)")
}

// ── env ───────────────────────────────────────────────────────────────────────

func (l *Layer) env() map[string]string {
	if l.envFn == nil {
		return map[string]string{}
	}
	return l.envFn()
}
