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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"     // Caddyfile lexer/parser
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile" // the "caddyfile" adapter
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/caddy-dns/cloudflare"
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
// assembled from every service's routing files.
func (e *Edge) Start(ctx context.Context) error {
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

	// A socket left by an agent that died would make the admin listener fail.
	if sock := AdminSocket(e.Root); sock != "" {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(sock)
		}
	}
	if err := e.Reload(); err != nil {
		_ = e.Stop()
		return err
	}
	return nil
}

func (e *Edge) adapt() ([]byte, error) {
	envFn := e.Env
	if envFn == nil {
		envFn = func() (map[string]string, error) { return DefaultEnv(e.Root) }
	}
	env, err := envFn()
	if err != nil {
		return nil, fmt.Errorf("building caddy env: %w", err)
	}
	assemble := e.assemble
	if assemble == nil {
		assemble = Assemble
	}
	text, err := assemble(e.Root)
	if err != nil {
		return nil, err
	}
	return adaptText(text, env)
}

// Reload re-assembles and loads the config in-process. A config Caddy
// rejects is ErrInvalidConfig, and the running config keeps serving.
func (e *Edge) Reload() error {
	cfg, err := e.adapt()
	if err != nil {
		return err
	}
	if err := caddy.Load(cfg, true); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
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
