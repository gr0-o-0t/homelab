package remote

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSOCKS5 is a minimal RFC 1928/1929 proxy: username/password auth,
// CONNECT only. It records each authenticated target and pipes to it.
type fakeSOCKS5 struct {
	ln      net.Listener
	user    string
	pass    string
	mu      sync.Mutex
	targets []string
	authBad int
}

func newFakeSOCKS5(t *testing.T, user, pass string) *fakeSOCKS5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &fakeSOCKS5{ln: ln, user: user, pass: pass}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s
}

func (s *fakeSOCKS5) Targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targets...)
}

func (s *fakeSOCKS5) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	_, _ = c.Write([]byte{5, 2}) // username/password
	// RFC 1929: ver, ulen, user, plen, pass
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil {
		return
	}
	user := make([]byte, b[1])
	_, _ = io.ReadFull(c, user)
	pl := make([]byte, 1)
	_, _ = io.ReadFull(c, pl)
	pass := make([]byte, pl[0])
	_, _ = io.ReadFull(c, pass)
	if string(user) != s.user || string(pass) != s.pass {
		s.mu.Lock()
		s.authBad++
		s.mu.Unlock()
		_, _ = c.Write([]byte{1, 1})
		return
	}
	_, _ = c.Write([]byte{1, 0})
	// request: ver, cmd, rsv, atyp, addr, port
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		_, _ = io.ReadFull(c, ip)
		host = net.IP(ip).String()
	case 3:
		l := make([]byte, 1)
		_, _ = io.ReadFull(c, l)
		name := make([]byte, l[0])
		_, _ = io.ReadFull(c, name)
		host = string(name)
	default:
		return
	}
	pb := make([]byte, 2)
	_, _ = io.ReadFull(c, pb)
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

// Inside the leader agent the relay reaches other agents only through the
// tailnet node's SOCKS5 proxy, which HOMELAB_TS_SOCKS(_CRED) name.
func TestClient_DialsThroughSOCKS5WhenSet(t *testing.T) {
	e := newTestServer(t, nil)
	proxy := newFakeSOCKS5(t, "tsnet", "proxy-cred")
	t.Setenv(SocksEnv, proxy.ln.Addr().String())
	t.Setenv(SocksCredEnv, "proxy-cred")

	code, out, _, err := run(t, e.client(testSecret, "1.4.2"), RunOptions{Argv: []string{"status"}})
	require.NoError(t, err)
	assert.Equal(t, 3, code)
	assert.Equal(t, "hello from the agent\n", out)
	assert.Equal(t, []string{strings.TrimPrefix(e.srv.URL, "https://")}, proxy.Targets())
}

func TestClient_DialsDirectlyWithoutSOCKSEnv(t *testing.T) {
	e := newTestServer(t, nil)
	proxy := newFakeSOCKS5(t, "tsnet", "proxy-cred")
	t.Setenv(SocksEnv, proxy.ln.Addr().String())
	t.Setenv(SocksCredEnv, "") // both are needed

	code, _, _, err := run(t, e.client(testSecret, "1.4.2"), RunOptions{Argv: []string{"status"}})
	require.NoError(t, err)
	assert.Equal(t, 3, code)
	assert.Empty(t, proxy.Targets())
}

// The proxy credential reaches the children (and only them) when set, and
// is never logged.
func TestRemote_ChildEnvCarriesTailnetProxy(t *testing.T) {
	e := newTestServer(t, func(c *ServerConfig) { c.TSSocks, c.TSSocksCred = "127.0.0.1:1080", "proxy-cred-SECRET" })
	_, out, _, err := run(t, e.client(testSecret, "1.4.2"), RunOptions{Argv: []string{"ls", "-q"}})
	require.NoError(t, err)
	assert.Contains(t, out, SocksEnv+"=127.0.0.1:1080\n")
	assert.Contains(t, out, SocksCredEnv+"=proxy-cred-SECRET\n")
	assert.NotContains(t, e.logs.String(), "proxy-cred-SECRET")
}
