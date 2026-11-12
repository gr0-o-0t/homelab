package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// TSSocks and TSSocksCred are the leader's tailnet SOCKS5 proxy and its
	// credential, the child's HOMELAB_TS_SOCKS and HOMELAB_TS_SOCKS_CRED:
	// the exec/logs relay dials other agents through it. Never logged.
	TSSocks, TSSocksCred string
	// Path is the child's PATH (default: the agent's own).
	Path string

	// Resolve maps argv to its canonical command path through the cobra
	// tree (cmd.ResolveCommand). Without it every command is refused.
	Resolve Resolver
	// AllowInternal are the canonical paths of Internal commands this server
	// runs for a client: a worker agent's "agent self-replace", through
	// which the leader's upgrade replaces it. The leader allows none.
	AllowInternal map[string]bool

	// KillGrace is how long a child gets between SIGTERM and SIGKILL once
	// its client is gone (default 10s).
	KillGrace time.Duration
	// PingInterval is how often the client is pinged; a client silent for
	// twice that (no frame, no pong) is gone (default 20s).
	PingInterval time.Duration
	// MaxSessions caps concurrent connections past auth (default 32): the
	// front ends' polling and every `logs -f` hold one each.
	MaxSessions int
	// Lock is the server-wide lock Mutating commands take: a channel of
	// capacity 1, holding a token while taken. Given, its owner can hold it
	// too (the leader's bring-up of services at boot); nil makes a new one.
	Lock chan struct{}
	// Logf logs connection events (default log.Printf). Never secrets.
	Logf func(format string, args ...any)
	// DetachLog receives the output of a Detached command once its client
	// is gone (default os.Stderr: the agent's log).
	DetachLog io.Writer
}

// Server runs forwarded homelab commands.
type Server struct {
	cfg      ServerConfig
	lock     chan struct{} // the one server-wide lock for Mutating commands
	sessions chan struct{} // one slot per running session
	upgrader websocket.Upgrader
}

const (
	handshakeTimeout = 10 * time.Second
	drainTimeout     = 2 * time.Second
	maxFrame         = 1 << 20
	stdinFrames      = 16
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
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 20 * time.Second
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 32
	}
	if cfg.Resolve == nil {
		cfg.Resolve = refuseAll
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.DetachLog == nil {
		cfg.DetachLog = os.Stderr
	}
	if cfg.Lock == nil {
		cfg.Lock = make(chan struct{}, 1)
	}
	return &Server{cfg: cfg, lock: cfg.Lock, sessions: make(chan struct{}, cfg.MaxSessions),
		upgrader: websocket.Upgrader{HandshakeTimeout: handshakeTimeout}}
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
	// writeWait bounds each write (0: none), so a client that stopped
	// reading cannot wedge the session — and the lock — on a full socket.
	writeWait time.Duration
}

func (ss *session) write(kind int, b []byte) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.writeWait > 0 {
		_ = ss.conn.SetWriteDeadline(time.Now().Add(ss.writeWait))
	}
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

// stdinQueue carries the client's stdin frames from the connection's reader
// to the child.
type stdinQueue struct {
	ch chan []byte
	// clean is set, before ch is closed, when the client said its stdin
	// ended (its EOF frame); otherwise the server stopped delivering it.
	clean bool
}

func newStdinQueue() *stdinQueue { return &stdinQueue{ch: make(chan []byte, stdinFrames)} }

// end closes the queue; clean says whether the client ended it.
func (q *stdinQueue) end(clean bool) {
	q.clean = clean
	close(q.ch)
}

// chanReader reads the stdin frames the connection's reader queues. It ends
// with io.EOF when the client ended its stdin, and io.ErrUnexpectedEOF when
// the stream was cut (client gone, or the child stalled on it too long).
type chanReader struct {
	q   *stdinQueue
	buf []byte
}

func (r *chanReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		b, ok := <-r.q.ch
		if !ok {
			if r.q.clean { // the close happened before this receive: no race
				return 0, io.EOF
			}
			return 0, io.ErrUnexpectedEOF
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
		s.cfg.Logf("remote: upgrade from %s: %q", r.RemoteAddr, err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	conn.SetReadLimit(maxFrame)
	ss := &session{conn: conn, writeWait: 2 * s.cfg.PingInterval}

	select {
	case s.sessions <- struct{}{}:
		defer func() { <-s.sessions }()
	default:
		s.cfg.Logf("remote: refused %s: %d sessions already running", r.RemoteAddr, s.cfg.MaxSessions)
		ss.finish(2, fmt.Sprintf("the agent is busy: %d homelab commands are already running — try again shortly", s.cfg.MaxSessions))
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var h Handshake
	if err := conn.ReadJSON(&h); err != nil {
		s.cfg.Logf("remote: bad handshake from %s: %q", r.RemoteAddr, err.Error())
		return
	}
	path, code, err := validate(h, s.cfg.Version, s.cfg.Resolve, s.cfg.AllowInternal)
	if err != nil {
		// Client-supplied strings are quoted: no forged log lines.
		s.cfg.Logf("remote: refused %q from %s: %q", CommandName(h.Argv), r.RemoteAddr, err.Error())
		ss.finish(code, err.Error())
		return
	}
	// Only the command path: arguments can carry values (setup --set).
	name := strings.Join(path, " ")
	s.cfg.Logf("remote: %s runs %q", r.RemoteAddr, name)

	// A client silent for two ping intervals — no frame, no pong — is gone.
	alive := func() { _ = conn.SetReadDeadline(time.Now().Add(2 * s.cfg.PingInterval)) }
	alive()
	conn.SetPongHandler(func(string) error { alive(); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := newStdinQueue()
	ch := &child{rows: h.Rows, cls: h.Cols}
	// Closed once the command runs: until then (a wait for the lock) its
	// stdin is not read, and the client is held back rather than cut off.
	started := make(chan struct{})
	go s.readLoop(ctx, conn, stdin, started, ch, cancel, alive)
	go keepAlive(ctx, ss, s.cfg.PingInterval, cancel)

	if Locks(path) {
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

	detach := DetachOnDisconnect(path)
	close(started)
	code, err = s.runChild(ctx, ss, h, ch, &chanReader{q: stdin}, detach)
	if ctx.Err() != nil {
		if detach {
			s.cfg.Logf("remote: %s disconnected; %q ran to completion (exit %d)", r.RemoteAddr, name, code)
		} else {
			s.cfg.Logf("remote: %s disconnected; %q stopped", r.RemoteAddr, name)
		}
		return
	}
	msg := ""
	if err != nil {
		code, msg = 1, err.Error()
	}
	ss.finish(code, msg)
}

// readLoop handles client frames until the connection fails or the client
// falls silent (read deadline), then cancels — which stops the child.
func (s *Server) readLoop(ctx context.Context, conn *websocket.Conn, stdin *stdinQueue, started <-chan struct{},
	ch *child, cancel context.CancelFunc, alive func()) {
	defer cancel()
	open := true
	defer func() {
		if open {
			stdin.end(false) // the connection failed or fell silent: cut
		}
	}()
	for {
		// Rearmed per frame, and after a wait on a full stdin queue, so time
		// spent waiting on the child is not held against the client.
		alive()
		kind, b, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch kind {
		case websocket.BinaryMessage:
			if !open {
				continue // stdin was closed: discarded
			}
			if !s.queueStdin(ctx, stdin, b, started) {
				if ctx.Err() != nil {
					return
				}
				// The child is not reading its stdin. Waiting longer would
				// stop this loop noticing a vanished client (deadlines and
				// pongs are only seen while reading) and so hold the lock:
				// cut its stdin instead, and drop what follows.
				stdin.end(false)
				open = false
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
				stdin.end(true)
				open = false
			}
		}
	}
}

// queueStdin queues b for the child, reporting false when ctx ended or the
// child, once running, left a full queue unread for two ping intervals.
// Before the command runs (a wait for the lock) it waits without limit: the
// client is back-pressured, and its stream is not cut for a wait it cannot
// help.
func (s *Server) queueStdin(ctx context.Context, stdin *stdinQueue, b []byte, started <-chan struct{}) bool {
	select {
	case stdin.ch <- b:
		return true
	case <-ctx.Done():
		return false
	case <-started:
	}
	t := time.NewTimer(2 * s.cfg.PingInterval)
	defer t.Stop()
	select {
	case stdin.ch <- b:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

// keepAlive pings the client so a vanished one is noticed even while the
// child is silent.
func keepAlive(ctx context.Context, ss *session, every time.Duration, cancel context.CancelFunc) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ss.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(every)); err != nil {
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
	env := []string{
		"HOMELAB_AGENT=1",
		"HOMELAB_VAULT=" + s.cfg.Vault,
		"HOMELAB_VAULT_KEY=" + s.cfg.VaultKey,
		"HOMELAB_CADDY_ADMIN=" + s.cfg.CaddyAdmin,
		"PATH=" + s.cfg.Path,
		"HOME=" + s.cfg.StateRoot,
		"TERM=" + term,
		"LANG=C.UTF-8",
	}
	if s.cfg.TSSocks != "" && s.cfg.TSSocksCred != "" {
		env = append(env, SocksEnv+"="+s.cfg.TSSocks, SocksCredEnv+"="+s.cfg.TSSocksCred)
	}
	return env
}

// detachWriter streams to the client while it is there, and to the agent
// log once it is gone (ctx done, or a write failed), never failing: a
// detached child must not block on, or die of, a full or broken pipe.
type detachWriter struct {
	ctx    context.Context
	client io.Writer
	log    io.Writer
	mu     sync.Mutex
	gone   bool
}

func (w *detachWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.gone && w.ctx.Err() == nil {
		if _, err := w.client.Write(p); err == nil {
			return len(p), nil
		}
	}
	w.gone = true
	_, _ = w.log.Write(p)
	return len(p), nil
}

// runChild runs the command to completion, or until ctx is cancelled, which
// stops it: SIGTERM to its process group, SIGKILL after KillGrace. A
// detached command is not stopped: it runs to completion, its output going
// to DetachLog once the client is gone.
func (s *Server) runChild(ctx context.Context, ss *session, h Handshake, ch *child, stdin io.Reader, detach bool) (int, error) {
	out := func(stderr bool) io.Writer {
		w := frameWriter{ss: ss, stderr: stderr}
		if !detach {
			return w
		}
		return &detachWriter{ctx: ctx, client: w, log: s.cfg.DetachLog}
	}
	args := []string{"--config-dir", s.cfg.StateDir}
	if !h.TTY {
		args = append(args, "--no-color")
	}
	args = append(args, h.Argv...)
	cmd := exec.Command(s.cfg.Binary, args...) //nolint:gosec // our own binary; argv validated
	cmd.Env = s.childEnv(h)
	cmd.Dir = s.cfg.StateRoot

	// cut records a stdin stream cut short (pipe path): the command was
	// stopped for it, unless it had already been reaped.
	var cut struct {
		sync.Mutex
		happened, reaped bool
	}
	// outDone closes once the output is drained; closeOut stops the drain.
	outDone := make(chan struct{})
	var closeOut func()
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
		closeOut = func() { _ = ptmx.Close() }
		defer closeOut()
		ch.mu.Lock()
		ch.ptmx = ptmx
		ch.mu.Unlock()
		go func() { _, _ = io.Copy(ptmx, stdin) }()
		if detach {
			// A detached child outlives its client and is never signalled:
			// one waiting at a prompt (restore's confirm) would wait forever,
			// holding the lock. Ending the copy above gives a pty no EOF, so
			// type one: ^U drops any partial line, ^D on the empty line is
			// EOF in canonical mode, and the prompt answers no.
			finished := make(chan struct{})
			defer close(finished)
			go func() {
				select {
				case <-ctx.Done():
					_, _ = ptmx.Write([]byte{0x15, 0x04})
				case <-finished:
				}
			}()
		}
		go func() {
			defer close(outDone)
			_, _ = io.Copy(out(false), ptmx)
		}()
	} else {
		// Own pipes rather than cmd.Stdout = writer, so Wait returns when
		// the process exits, before its output is drained: lingering
		// grandchildren holding the pipes are killed first.
		outR, outW, err := os.Pipe()
		if err != nil {
			return 1, err
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			_ = outR.Close()
			_ = outW.Close()
			return 1, err
		}
		closeOut = func() { _ = outR.Close(); _ = errR.Close() }
		defer closeOut()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout, cmd.Stderr = outW, errW
		in, err := cmd.StdinPipe()
		if err != nil {
			_ = outW.Close()
			_ = errW.Close()
			return 1, err
		}
		err = cmd.Start()
		_ = outW.Close()
		_ = errW.Close()
		if err != nil {
			return 1, err
		}
		go func() {
			_, err := io.Copy(in, stdin)
			if errors.Is(err, io.ErrUnexpectedEOF) && !detach {
				// A cut stream must not reach the command as a clean EOF
				// (a short tar would install, a short image load): stop
				// it before its stdin closes. Wait closes the pipe.
				cut.Lock()
				if !cut.reaped {
					cut.happened = true
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
				cut.Unlock()
				return
			}
			_ = in.Close()
		}()
		errDone := make(chan struct{})
		go func() {
			defer close(errDone)
			_, _ = io.Copy(out(true), errR)
		}()
		go func() {
			defer close(outDone)
			_, _ = io.Copy(out(false), outR)
			<-errDone
		}()
	}

	exited := make(chan struct{})
	go func() {
		select {
		case <-exited:
		case <-ctx.Done():
			if detach {
				return // runs to completion; the lock is held until then
			}
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
	cut.Lock()
	cut.reaped = true
	stdinCut := cut.happened
	cut.Unlock()
	// Nothing the command started outlives it, nor keeps running once the
	// lock it held is released.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	// Output ends (EOF, or EIO on a pty) once every holder is gone; one that
	// escaped the process group must not hold the exit.
	select {
	case <-outDone:
	case <-time.After(drainTimeout):
		closeOut() // not waited on: the drain may be stuck writing to a dead client
	}
	var ee *exec.ExitError
	switch {
	case stdinCut:
		return 1, errors.New("the command's input was cut off before its end (the connection was lost, or the command stalled reading it); the command was stopped")
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
