// Package edge runs the agent's network edge in-process: Caddy (with caddy-l4
// and the Cloudflare DNS provider) serving on the agent's own Tailscale node
// (tsnet), instead of separate caddy and tailscale containers.
//
// Two Caddy networks are registered: "hl-ts" (stream) and "hl-tsu" (datagram).
// A tailnet site `bind hl-ts/homelab`, a raw tcp port listens on
// `hl-ts/homelab:<port>` and a raw udp port on `hl-tsu/homelab:<port>`
// (internal/network.TailnetBind*). The host part is ignored: there is one
// tsnet node per agent.
//
// The running Caddy exposes its admin API on a unix socket (AdminSocket), the
// only way another process — `homelab enable` in the CLI — can reload it.
package edge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/caddy-dns/cloudflare"
	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"     // Caddyfile lexer/parser
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile" // the "caddyfile" adapter
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/mholt/caddy-l4"
	"tailscale.com/tsnet"
)

// Network names registered with Caddy; see internal/network.TailnetBind.
const (
	netStream = "hl-ts"
	netPacket = "hl-tsu"
)

// ListenTCP opens a stream listener on the tailnet node's port.
type ListenTCP func(port int) (net.Listener, error)

// ListenUDP opens a datagram socket on the tailnet node's port.
type ListenUDP func(port int) (net.PacketConn, error)

// active holds the listen funcs of the running edge. The Caddy networks are
// process-global and registered once, so they dispatch through it.
var active struct {
	sync.Mutex
	tcp ListenTCP
	udp ListenUDP
}

func setListeners(tcp ListenTCP, udp ListenUDP) {
	active.Lock()
	active.tcp, active.udp = tcp, udp
	active.Unlock()
}

func init() {
	caddy.RegisterNetwork(netStream, func(_ context.Context, _, _, portRange string, offset uint, _ net.ListenConfig) (any, error) {
		port, err := portOf(portRange, offset)
		if err != nil {
			return nil, err
		}
		active.Lock()
		listen := active.tcp
		active.Unlock()
		if listen == nil {
			return nil, errors.New("the tailnet node is not up")
		}
		return sockets.listener(fmt.Sprintf("%s:%d", netStream, port), func() (net.Listener, error) { return listen(port) })
	})
	caddy.RegisterNetwork(netPacket, func(_ context.Context, _, _, portRange string, offset uint, _ net.ListenConfig) (any, error) {
		port, err := portOf(portRange, offset)
		if err != nil {
			return nil, err
		}
		active.Lock()
		listen := active.udp
		active.Unlock()
		if listen == nil {
			return nil, errors.New("the tailnet node is not up")
		}
		return sockets.packetConn(fmt.Sprintf("%s:%d", netPacket, port), func() (net.PacketConn, error) { return listen(port) })
	})
	// An HTTPS site bound to hl-ts also gets an HTTP/3 listener; without this
	// mapping Caddy refuses the whole config ("cannot handle HTTP/3").
	caddyhttp.RegisterNetworkHTTP3(netStream, netPacket)
}

// portOf resolves Caddy's port range ("443" or "8000-8010") plus offset.
func portOf(portRange string, offset uint) (int, error) {
	start, _, _ := strings.Cut(portRange, "-")
	p, err := strconv.ParseUint(start, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("port %q: %w", portRange, err)
	}
	port := p + uint64(offset)
	if port > 65535 {
		return 0, fmt.Errorf("port %d out of range", port)
	}
	return int(port), nil
}

// Edge is the agent's Caddy on its tailnet node. Only one runs per process:
// Caddy itself is process-global.
type Edge struct {
	Root       string // state root: caddy/sites, caddy/l4, config.yaml
	TSAuthKey  string
	TSHostname string // default "homelab"
	TSStateDir string // default <Root>/tsnet

	// Env fills the Caddyfile's {$VAR} placeholders; nil = DefaultEnv(Root).
	Env func() (map[string]string, error)

	listenTCP ListenTCP // set by newForTest: no tsnet
	listenUDP ListenUDP
	assemble  func(root string) (string, error)
	ts        *tsnet.Server
}

// newForTest returns an edge that serves the registered networks with the
// given listeners instead of a tsnet node.
func newForTest(root string, tcp ListenTCP, udp ListenUDP) *Edge {
	return &Edge{Root: root, listenTCP: tcp, listenUDP: udp}
}

// Start brings the tailnet node up, waits for its IPv4, and loads the config
// assembled from every service's routing files. A config that does not load
// leaves the edge up on an admin-only config and returns ErrConfigNotLoaded;
// any other error leaves nothing running (ErrAnotherAgent: do not retry).
func (e *Edge) Start(ctx context.Context) error {
	if err := claimAdminSocket(AdminSocket(e.Root)); err != nil {
		return err
	}
	tcp, udp := e.listenTCP, e.listenUDP
	if tcp == nil {
		dir := e.TSStateDir
		if dir == "" {
			dir = filepath.Join(e.Root, "tsnet")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		host := e.TSHostname
		if host == "" {
			host = "homelab"
		}
		srv := &tsnet.Server{
			Dir:      dir,
			Hostname: host,
			AuthKey:  e.TSAuthKey,
			Logf:     func(string, ...any) {},
			UserLogf: log.Printf,
		}
		if _, err := srv.Up(ctx); err != nil {
			_ = srv.Close()
			return fmt.Errorf("starting tailnet node: %w", err)
		}
		ip4, _ := srv.TailscaleIPs()
		if !ip4.IsValid() {
			_ = srv.Close()
			return errors.New("tailnet node has no IPv4 address")
		}
		e.ts = srv
		tcp = func(port int) (net.Listener, error) { return srv.Listen("tcp", ":"+strconv.Itoa(port)) }
		udp = func(port int) (net.PacketConn, error) {
			return srv.ListenPacket("udp", net.JoinHostPort(ip4.String(), strconv.Itoa(port)))
		}
	}
	setListeners(tcp, udp)

	if err := e.Reload(); err != nil {
		// The routing files (or the env they need) are broken. Stopping
		// here would take the tailnet node and the admin socket with it —
		// and with them every way to repair the config. Run Caddy with
		// only its admin API instead; a reload of a fixed config (the
		// CLI's, over the admin socket, or Reload) brings the sites up.
		if lerr := e.loadAdminOnly(); lerr != nil {
			_ = e.Stop()
			return fmt.Errorf("%w; the admin-only fallback failed too: %w", err, lerr)
		}
		return fmt.Errorf("%w: %w", ErrConfigNotLoaded, err)
	}
	return nil
}

// ErrConfigNotLoaded is a Start whose edge is up — tailnet node, admin
// socket — but whose routing config did not load: Caddy serves its admin API
// only until a fixed config is loaded. Stop it as usual.
var ErrConfigNotLoaded = errors.New("the edge is up but its config did not load")

// ErrAnotherAgent is a Start refused because another agent's Caddy answers
// on the admin socket. Retrying cannot help.
var ErrAnotherAgent = errors.New("another agent is running")

// loadAdminOnly runs Caddy with its admin API on the unix socket and no
// apps.
func (e *Edge) loadAdminOnly() error {
	text := fmt.Sprintf("{\n\tadmin %q\n\tstorage file_system %q\n\tpersist_config off\n}\n",
		"unix/"+AdminSocket(e.Root)+"|0600", DataDir(e.Root))
	cfg, err := adaptText(text, nil)
	if err != nil {
		return err
	}
	return caddy.Load(cfg, true)
}

// claimAdminSocket makes sock free for this agent's Caddy. Something
// answering on it is another agent on the same state: refuse rather than
// unlink its socket. A socket nobody answers on (ECONNREFUSED) was left by
// an agent that died and is removed; with no file (ENOENT) there is nothing
// to do. Any other failure leaves the file alone.
func claimAdminSocket(sock string) error {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	switch {
	case err == nil:
		_ = c.Close()
		return fmt.Errorf("%w: its Caddy admin API answers on %s", ErrAnotherAgent, sock)
	case errors.Is(err, syscall.ENOENT):
		return nil
	case errors.Is(err, syscall.ECONNREFUSED):
		if fi, err := os.Lstat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", sock)
		}
		if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing stale admin socket %s: %w", sock, err)
		}
		return nil
	default:
		return fmt.Errorf("checking admin socket %s: %w", sock, err)
	}
}

// Tailnet is the edge's tsnet node (nil before Start, or in tests): the
// agent serves the remote CLI on it too, so the leader is one tailnet node.
func (e *Edge) Tailnet() *tsnet.Server { return e.ts }

// SOCKS is the tailnet node's credentialed SOCKS5 proxy on 127.0.0.1
// (tsnet Loopback, user "tsnet"): the agent's child processes dial other
// agents through it, since the container itself has no route to the
// tailnet or its MagicDNS names. Neither value may be logged.
func (e *Edge) SOCKS() (addr, cred string, err error) {
	if e.ts == nil {
		return "", "", errors.New("the tailnet node is not up")
	}
	addr, cred, _, err = e.ts.Loopback()
	return addr, cred, err
}

// TailnetIPv4 is the tailnet node's IPv4 (invalid before Start, or in tests).
func (e *Edge) TailnetIPv4() netip.Addr {
	if e.ts == nil {
		return netip.Addr{}
	}
	ip4, _ := e.ts.TailscaleIPs()
	return ip4
}

// ListenTLS listens on the tailnet node with its *.ts.net certificate.
func (e *Edge) ListenTLS(network, addr string) (net.Listener, error) {
	if e.ts == nil {
		return nil, errors.New("the tailnet node is not up")
	}
	return e.ts.ListenTLS(network, addr)
}

func (e *Edge) adapt() ([]byte, error) {
	cfg, _, err := e.adaptEnv()
	return cfg, err
}

// adaptEnv is adapt, also returning the env the config was filled from.
func (e *Edge) adaptEnv() ([]byte, map[string]string, error) {
	envFn := e.Env
	if envFn == nil {
		envFn = func() (map[string]string, error) { return DefaultEnv(e.Root) }
	}
	env, err := envFn()
	if err != nil {
		return nil, nil, fmt.Errorf("building caddy env: %w", err)
	}
	assemble := e.assemble
	if assemble == nil {
		assemble = Assemble
	}
	text, err := assemble(e.Root)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := adaptText(text, env)
	return cfg, env, err
}

// Reload re-assembles and loads the config in-process. A config Caddy
// rejects is ErrInvalidConfig, and the running config keeps serving.
func (e *Edge) Reload() error {
	cfg, env, err := e.adaptEnv()
	if err != nil {
		return err
	}
	return loadInProcess(cfg, env)
}

// loadInProcess loads cfg into this process's Caddy. A config it rejects is
// ErrInvalidConfig, redacted as adapt errors are: provisioning errors may
// quote a secret (caddy-dns/cloudflare quotes a token it finds malformed).
func loadInProcess(cfg []byte, env map[string]string) error {
	if err := caddy.Load(cfg, true); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidConfig, redact(err.Error(), env))
	}
	return nil
}

// Stop stops Caddy — its apps and its admin socket — then the tailnet node.
func (e *Edge) Stop() error {
	// caddy.Stop leaves the admin server running; loading a config with admin
	// disabled stops it (and every app) first.
	errs := []error{caddy.Load([]byte(`{"admin":{"disabled":true,"config":{"persist":false}}}`), true), caddy.Stop()}
	setListeners(nil, nil)
	if e.ts != nil {
		errs = append(errs, e.ts.Close())
		e.ts = nil
	}
	return errors.Join(errs...)
}
