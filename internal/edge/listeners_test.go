package edge

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dyingListener fails its first Accept: the tailnet socket went away under a
// running config.
type dyingListener struct {
	closed atomic.Int32
}

var errSocketDied = errors.New("socket died")

func (l *dyingListener) Accept() (net.Conn, error) { return nil, errSocketDied }
func (l *dyingListener) Close() error              { l.closed.Add(1); return nil }
func (l *dyingListener) Addr() net.Addr            { return &net.TCPAddr{} }

// A shared socket whose listener died is evicted, so the next reload opens a
// fresh one instead of handing out handles on the dead socket forever.
func TestPool_DeadListenerIsReopened(t *testing.T) {
	p := &pool{streams: map[string]*sharedListener{}, packets: map[string]*sharedPacketConn{}}
	dead := &dyingListener{}
	old, err := p.listener("hl-ts:22", func() (net.Listener, error) { return dead, nil })
	require.NoError(t, err)
	defer func() { _ = old.Close() }()
	_, err = old.Accept()
	require.ErrorIs(t, err, errSocketDied, "the old config's handle sees the failure")

	fresh, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	opened := 0
	h, err := p.listener("hl-ts:22", func() (net.Listener, error) { opened++; return fresh, nil })
	require.NoError(t, err)
	defer func() { _ = h.Close() }()
	assert.Equal(t, 1, opened, "the dead socket is not shared")
	assert.Positive(t, dead.closed.Load(), "the dead socket is closed when evicted")

	go func() {
		if c, err := net.Dial("tcp", fresh.Addr().String()); err == nil {
			_ = c.Close()
		}
	}()
	c, err := h.Accept()
	require.NoError(t, err)
	_ = c.Close()

	// Releasing the old handle must not drop the new socket from the pool.
	require.NoError(t, old.Close())
	h2, err := p.listener("hl-ts:22", func() (net.Listener, error) {
		t.Error("the live socket must be shared, not reopened")
		return nil, errors.New("unexpected open")
	})
	require.NoError(t, err)
	_ = h2.Close()
}

// dyingPacketConn fails its first read.
type dyingPacketConn struct {
	net.PacketConn
	closed atomic.Int32
}

func (c *dyingPacketConn) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, errSocketDied }
func (c *dyingPacketConn) Close() error                           { c.closed.Add(1); return nil }
func (c *dyingPacketConn) LocalAddr() net.Addr                    { return &net.UDPAddr{} }

func TestPool_DeadPacketConnIsReopened(t *testing.T) {
	p := &pool{streams: map[string]*sharedListener{}, packets: map[string]*sharedPacketConn{}}
	dead := &dyingPacketConn{}
	old, err := p.packetConn("hl-tsu:53", func() (net.PacketConn, error) { return dead, nil })
	require.NoError(t, err)
	defer func() { _ = old.Close() }()
	_ = old.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = old.ReadFrom(make([]byte, 8))
	require.ErrorIs(t, err, errSocketDied)

	fresh, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	opened := 0
	h, err := p.packetConn("hl-tsu:53", func() (net.PacketConn, error) { opened++; return fresh, nil })
	require.NoError(t, err)
	defer func() { _ = h.Close() }()
	assert.Equal(t, 1, opened)
	assert.Positive(t, dead.closed.Load())
}
