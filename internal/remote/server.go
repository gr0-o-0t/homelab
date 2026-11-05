package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// ServerConfig is what the agent hands the runner.
type ServerConfig struct {
	StateDir  string // passed to every child as --config-dir
	StateRoot string // the child's HOME and working directory
	Secret    string // the cluster secret; the bearer token is derived from it
	Version   string // the agent's version, compared with the client's

	// Binary is the homelab executable children run (default os.Args[0]).
	Binary string
	// Vault, VaultKey and CaddyAdmin become the child's HOMELAB_VAULT,
	// HOMELAB_VAULT_KEY and HOMELAB_CADDY_ADMIN.
	Vault, VaultKey, CaddyAdmin string
	// Path is the child's PATH (default: the agent's own).
	Path string

	// KillGrace is how long a child gets between SIGTERM and SIGKILL once
	// its client is gone (default 10s).
	KillGrace time.Duration
	// Logf logs connection events (default log.Printf). Never secrets.
	Logf func(format string, args ...any)
}

// Server runs forwarded homelab commands.
type Server struct {
	cfg      ServerConfig
	lock     chan struct{} // the one server-wide lock for Mutating commands
	upgrader websocket.Upgrader
}

const (
	handshakeTimeout = 10 * time.Second
	pingInterval     = 20 * time.Second
	drainTimeout     = 2 * time.Second
	maxFrame         = 1 << 20
)

// NewServer returns a runner for cfg.
func NewServer(cfg ServerConfig) *Server {
	if cfg.Binary == "" {
		cfg.Binary = os.Args[0]
	}
	if cfg.Path == "" {
		cfg.Path = os.Getenv("PATH")
	}
	if cfg.Path == "" {
		cfg.Path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = 10 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Server{cfg: cfg, lock: make(chan struct{}, 1), upgrader: websocket.Upgrader{HandshakeTimeout: handshakeTimeout}}
}

// Handler serves GET /v1/run.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Path, s.handleRun)
	return mux
}

// Serve accepts connections on ln (the agent's tsnet TLS listener) until it
// fails or is closed.
func (s *Server) Serve(ln net.Listener) error {
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: handshakeTimeout}
	return hs.Serve(ln)
}

// session is one websocket connection; gorilla allows one writer at a time.
type session struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (ss *session) write(kind int, b []byte) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.conn.WriteMessage(kind, b)
}

func (ss *session) control(c control) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return ss.write(websocket.TextMessage, b)
}

// finish sends the exit frame (with an error message, if any) and closes.
func (ss *session) finish(code int, msg string) {
	_ = ss.control(control{Exit: &code, Error: msg})
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_ = ss.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
}

// frameWriter sends everything written to it as output frames.
type frameWriter struct {
	ss     *session
	stderr bool
}

func (w frameWriter) Write(p []byte) (int, error) {
	var err error
	if w.stderr {
		err = w.ss.control(control{Stderr: p})
	} else {
		err = w.ss.write(websocket.BinaryMessage, p)
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// chanReader reads the stdin frames the connection's reader queues.
type chanReader struct {
	ch  <-chan []byte
	buf []byte
}

func (r *chanReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		b, ok := <-r.ch
		if !ok {
			return 0, io.EOF
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// child is the running command, for resize frames that may arrive early.
type child struct {
	mu        sync.Mutex
	ptmx      *os.File
	rows, cls uint16
}

func (c *child) resize(rows, cols uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows, c.cls = rows, cols
	if c.ptmx != nil {
		_ = pty.Setsize(c.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
	}
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if !authorized(r, s.cfg.Secret) {
		s.cfg.Logf("remote: unauthorized request from %s", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.cfg.Logf("remote: upgrade from %s: %v", r.RemoteAddr, err)
		return
	}
	defer func() { _ = conn.Close() }()
	conn.SetReadLimit(maxFrame)
	ss := &session{conn: conn}

	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var h Handshake
	if err := conn.ReadJSON(&h); err != nil {
		s.cfg.Logf("remote: bad handshake from %s: %v", r.RemoteAddr, err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if code, err := validate(h, s.cfg.Version); err != nil {
		s.cfg.Logf("remote: refused %q from %s: %v", CommandName(h.Argv), r.RemoteAddr, err)
		ss.finish(code, err.Error())
		return
	}
	name := CommandName(h.Argv)
	// Only the command name: arguments can carry values (setup --set).
	s.cfg.Logf("remote: %s runs %q", r.RemoteAddr, name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := make(chan []byte, 256)
	ch := &child{rows: h.Rows, cls: h.Cols}
	go s.readLoop(ctx, conn, stdin, ch, cancel)
	go keepAlive(ctx, ss, cancel)

	if Mutating[name] {
		select {
		case s.lock <- struct{}{}:
		default:
			s.notice(ss, h.TTY, "waiting for another homelab command to finish…")
			select {
			case s.lock <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		defer func() { <-s.lock }()
	}

	code, err := s.runChild(ctx, ss, h, ch, &chanReader{ch: stdin})
	if ctx.Err() != nil {
		s.cfg.Logf("remote: %s disconnected; %q stopped", r.RemoteAddr, name)
		return
	}
	msg := ""
	if err != nil {
		code, msg = 1, err.Error()
	}
	ss.finish(code, msg)
}

// readLoop handles client frames until the connection fails, then cancels.
func (s *Server) readLoop(ctx context.Context, conn *websocket.Conn, stdin chan<- []byte, ch *child, cancel context.CancelFunc) {
	defer cancel()
	open := true
	defer func() {
		if open {
			close(stdin)
		}
	}()
	for {
		kind, b, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch kind {
		case websocket.BinaryMessage:
			if open {
				select {
				case stdin <- b:
				case <-ctx.Done():
					return
				}
			}
		case websocket.TextMessage:
			var c control
			if json.Unmarshal(b, &c) != nil {
				continue
			}
			if c.Resize != nil {
				ch.resize(c.Resize[0], c.Resize[1])
			}
			if c.EOF && open {
				close(stdin)
				open = false
			}
		}
	}
}

// keepAlive pings the client so a vanished one is noticed even while the
// child is silent.
func keepAlive(ctx context.Context, ss *session, cancel context.CancelFunc) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ss.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(pingInterval)); err != nil {
				cancel()
				return
			}
		}
	}
}

func (s *Server) notice(ss *session, tty bool, msg string) {
	if tty {
		_ = ss.write(websocket.BinaryMessage, []byte(msg+"\r\n"))
		return
	}
	_ = ss.control(control{Stderr: []byte(msg + "\n")})
}

// childEnv is the child's whole environment: nothing of the agent's own is
// inherited beyond PATH.
func (s *Server) childEnv(h Handshake) []string {
	term := "dumb"
	if h.TTY && h.Term != "" && len(h.Term) <= 64 && !strings.ContainsAny(h.Term, "\x00\n=") {
		term = h.Term
	}
	return []string{
		"HOMELAB_AGENT=1",
		"HOMELAB_VAULT=" + s.cfg.Vault,
		"HOMELAB_VAULT_KEY=" + s.cfg.VaultKey,
		"HOMELAB_CADDY_ADMIN=" + s.cfg.CaddyAdmin,
		"PATH=" + s.cfg.Path,
		"HOME=" + s.cfg.StateRoot,
		"TERM=" + term,
		"LANG=C.UTF-8",
	}
}

// runChild runs the command to completion, or until ctx is cancelled, which
// stops it: SIGTERM to its process group, SIGKILL after KillGrace.
func (s *Server) runChild(ctx context.Context, ss *session, h Handshake, ch *child, stdin io.Reader) (int, error) {
	args := []string{"--config-dir", s.cfg.StateDir}
	if !h.TTY {
		args = append(args, "--no-color")
	}
	args = append(args, h.Argv...)
	cmd := exec.Command(s.cfg.Binary, args...) //nolint:gosec // our own binary; argv validated
	cmd.Env = s.childEnv(h)
	cmd.Dir = s.cfg.StateRoot

	var outDone chan struct{}
	if h.TTY {
		ch.mu.Lock()
		rows, cols := ch.rows, ch.cls
		ch.mu.Unlock()
		if rows == 0 || cols == 0 {
			rows, cols = 24, 80
		}
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols}) // new session: pgid = pid
		if err != nil {
			return 1, err
		}
		defer func() { _ = ptmx.Close() }()
		ch.mu.Lock()
		ch.ptmx = ptmx
		ch.mu.Unlock()
		go func() { _, _ = io.Copy(ptmx, stdin) }()
		outDone = make(chan struct{})
		go func() {
			defer close(outDone)
			_, _ = io.Copy(frameWriter{ss: ss}, ptmx)
		}()
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = frameWriter{ss: ss}
		cmd.Stderr = frameWriter{ss: ss, stderr: true}
		cmd.WaitDelay = drainTimeout
		in, err := cmd.StdinPipe()
		if err != nil {
			return 1, err
		}
		if err := cmd.Start(); err != nil {
			return 1, err
		}
		go func() {
			_, _ = io.Copy(in, stdin)
			_ = in.Close()
		}()
	}

	exited := make(chan struct{})
	go func() {
		select {
		case <-exited:
		case <-ctx.Done():
			pgid := cmd.Process.Pid
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(s.cfg.KillGrace):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
	}()
	err := cmd.Wait()
	close(exited)
	if outDone != nil {
		// The pty reads EIO once every holder of its tty is gone; a
		// background grandchild keeping it open must not hold the exit.
		select {
		case <-outDone:
		case <-time.After(drainTimeout):
		}
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	default:
		return 1, err
	}
}
