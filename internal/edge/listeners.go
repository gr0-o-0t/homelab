package edge

import (
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// Caddy starts a new config before it stops the old one, and only shares
// sockets it opened itself: a registered network is asked for the same port
// again while the old listener is still open. tsnet refuses a second listener
// on a port, so each socket is opened once and handed out as refcounted
// handles; the socket closes with its last handle.

type pool struct {
	mu      sync.Mutex
	streams map[string]*sharedListener
	packets map[string]*sharedPacketConn
}

var sockets = &pool{streams: map[string]*sharedListener{}, packets: map[string]*sharedPacketConn{}}

// listener returns a handle on the stream socket key, opening it with open if
// nothing holds it.
func (p *pool) listener(key string, open func() (net.Listener, error)) (net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.streams[key]
	if s != nil && closed(s.dead) {
		// The socket failed under the running config. Sharing it would
		// hand the next config a listener that never accepts again; evict
		// it (its remaining handles keep reporting the failure) and open a
		// new one. Closing it frees the port for that.
		delete(p.streams, key)
		_ = s.ln.Close()
		s = nil
	}
	if s == nil {
		ln, err := open()
		if err != nil {
			return nil, err
		}
		s = &sharedListener{ln: ln, conns: make(chan net.Conn), dead: make(chan struct{}), done: make(chan struct{})}
		go s.loop()
		p.streams[key] = s
	}
	s.refs++
	return &listenerHandle{s: s, closed: make(chan struct{}), release: func() { p.release(key, s) }}, nil
}

// closed reports whether ch is closed, without blocking.
func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (p *pool) release(key string, s *sharedListener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.refs--; s.refs == 0 {
		if p.streams[key] == s {
			delete(p.streams, key)
		}
		close(s.done)
		_ = s.ln.Close()
	}
}

type sharedListener struct {
	ln    net.Listener
	refs  int
	conns chan net.Conn
	dead  chan struct{} // the socket failed; err says why
	done  chan struct{} // the last handle closed
	err   error
}

func (s *sharedListener) loop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			s.err = err
			close(s.dead)
			return
		}
		select {
		case s.conns <- c:
		case <-s.done:
			_ = c.Close()
		}
	}
}

type listenerHandle struct {
	s       *sharedListener
	once    sync.Once
	closed  chan struct{}
	release func()
}

func (h *listenerHandle) Accept() (net.Conn, error) {
	select {
	case <-h.closed:
		return nil, net.ErrClosed
	default:
	}
	select {
	case c := <-h.s.conns:
		return c, nil
	case <-h.closed:
		return nil, net.ErrClosed
	case <-h.s.dead:
		return nil, h.s.err
	}
}

func (h *listenerHandle) Close() error {
	h.once.Do(func() { close(h.closed); h.release() })
	return nil
}

func (h *listenerHandle) Addr() net.Addr { return h.s.ln.Addr() }

// packetConn returns a handle on the datagram socket key, opening it with open
// if nothing holds it. Every handle reads from one reader goroutine; while a
// reload overlaps two handles, a datagram goes to whichever reads first.
func (p *pool) packetConn(key string, open func() (net.PacketConn, error)) (net.PacketConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.packets[key]
	if s != nil && closed(s.dead) {
		delete(p.packets, key) // failed socket: see listener
		_ = s.pc.Close()
		s = nil
	}
	if s == nil {
		pc, err := open()
		if err != nil {
			return nil, err
		}
		s = &sharedPacketConn{pc: pc, pkts: make(chan datagram), dead: make(chan struct{}), done: make(chan struct{})}
		go s.loop()
		p.packets[key] = s
	}
	s.refs++
	return &packetHandle{s: s, closed: make(chan struct{}), wake: make(chan struct{}),
		release: func() { p.releasePacket(key, s) }}, nil
}

func (p *pool) releasePacket(key string, s *sharedPacketConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.refs--; s.refs == 0 {
		if p.packets[key] == s {
			delete(p.packets, key)
		}
		close(s.done)
		_ = s.pc.Close()
	}
}

type datagram struct {
	b    []byte
	addr net.Addr
}

type sharedPacketConn struct {
	pc   net.PacketConn
	refs int
	pkts chan datagram
	dead chan struct{} // the socket failed; err says why
	done chan struct{} // the last handle closed
	err  error
}

func (s *sharedPacketConn) loop() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := s.pc.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.err = err
			close(s.dead)
			return
		}
		select {
		case s.pkts <- datagram{b: append([]byte(nil), buf[:n]...), addr: addr}:
		case <-s.done:
		}
	}
}

type packetHandle struct {
	s       *sharedPacketConn
	once    sync.Once
	closed  chan struct{}
	release func()

	mu       sync.Mutex
	deadline time.Time
	wake     chan struct{} // closed when the deadline changes
}

func (h *packetHandle) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		h.mu.Lock()
		dl, wake := h.deadline, h.wake
		h.mu.Unlock()
		var timeout <-chan time.Time
		var timer *time.Timer
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}
		select {
		case dg := <-h.s.pkts:
			stop(timer)
			return copy(p, dg.b), dg.addr, nil
		case <-h.closed:
			stop(timer)
			return 0, nil, net.ErrClosed
		case <-h.s.dead:
			stop(timer)
			return 0, nil, h.s.err
		case <-timeout:
			return 0, nil, os.ErrDeadlineExceeded
		case <-wake:
			stop(timer)
		}
	}
}

func stop(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}

func (h *packetHandle) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-h.closed:
		return 0, net.ErrClosed
	default:
	}
	return h.s.pc.WriteTo(p, addr)
}

func (h *packetHandle) Close() error {
	h.once.Do(func() { close(h.closed); h.release() })
	return nil
}

func (h *packetHandle) LocalAddr() net.Addr { return h.s.pc.LocalAddr() }

func (h *packetHandle) SetDeadline(t time.Time) error { return h.SetReadDeadline(t) }

func (h *packetHandle) SetReadDeadline(t time.Time) error {
	h.mu.Lock()
	h.deadline = t
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
	return nil
}

// SetWriteDeadline is a no-op: the socket is shared, and a write deadline on
// it would apply to every handle.
func (h *packetHandle) SetWriteDeadline(time.Time) error { return nil }
