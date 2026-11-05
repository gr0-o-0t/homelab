package edge

// These tests run the real embedded Caddy (process-global), so none of them is
// parallel and each stops Caddy in t.Cleanup. No tailnet: newForTest swaps the
// tsnet listeners for plain localhost sockets.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testEnv = map[string]string{
	"DOMAIN":               "example.test",
	"HOME_SUBDOMAIN":       "home",
	"ACME_EMAIL":           "admin@example.test",
	"CLOUDFLARE_API_TOKEN": "0123456789abcdefghijklmnopqrstuvwxyzABCD",
}

func fixedEnv() (map[string]string, error) { return testEnv, nil }

// freePort returns a port free for both tcp and udp on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := ln.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		_ = ln.Close()
		if err == nil {
			_ = pc.Close()
			return port
		}
	}
	t.Fatal("no free port")
	return 0
}

func localTCP(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func localUDP(port int) (net.PacketConn, error) {
	return net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func siteFile(root, svc string) string { return filepath.Join(root, "caddy", "sites", svc+".conf") }
func l4File(root, svc string) string   { return filepath.Join(root, "caddy", "l4", svc+".conf") }

func httpSite(port int, body string) string {
	return fmt.Sprintf("http://:%d {\n\tbind hl-ts/homelab\n\trespond %q\n}\n", port, body)
}

// testEdge returns an edge on localhost sockets whose catch-alls (:80, :8081
// in production) listen on free ports, with the admin socket in root.
func testEdge(t *testing.T, root string, rewrite ...func(string) string) *Edge {
	t.Helper()
	t.Setenv(AdminEnv, filepath.Join(root, "admin.sock"))
	// Caddy's instance id and any autosave land here, not in the real home.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(xdg, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(xdg, "data"))
	t.Cleanup(func() {
		assert.NoFileExists(t, filepath.Join(xdg, "config", "caddy", "autosave.json"),
			"the loaded config holds secrets and must never be autosaved")
	})
	c80, c8081 := freePort(t), freePort(t)
	e := newForTest(root, localTCP, localUDP)
	e.Env = fixedEnv
	e.assemble = func(r string) (string, error) {
		s, err := Assemble(r)
		s = strings.Replace(s, "http://:80 {", fmt.Sprintf("http://:%d {", c80), 1)
		s = strings.Replace(s, "http://:8081 {", fmt.Sprintf("http://:%d {", c8081), 1)
		for _, f := range rewrite {
			s = f(s)
		}
		return s, err
	}
	t.Cleanup(func() { _ = e.Stop() })
	return e
}

func get(t *testing.T, url string) (string, error) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test against tls internal
	}}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func mustGet(t *testing.T, url string) string {
	t.Helper()
	body, err := get(t, url)
	require.NoError(t, err)
	return body
}

// ── Assemble ──────────────────────────────────────────────────────────────────

func TestAssemble_InlinesL4AndSites(t *testing.T) {
	root := t.TempDir()
	t.Setenv(AdminEnv, "")
	writeFile(t, siteFile(root, "b"), "b.{$HOME_SUBDOMAIN}.{$DOMAIN} {\n\treverse_proxy b:80\n}\n")
	writeFile(t, siteFile(root, "a"), "a.{$HOME_SUBDOMAIN}.{$DOMAIN} {\n\treverse_proxy a:80\n}\n")
	writeFile(t, l4File(root, "gitea"), "hl-ts/homelab:22 {\n\troute {\n\t\tproxy gitea:2222\n\t}\n}\n")
	writeFile(t, filepath.Join(root, "caddy", "sites", "notes.txt"), "not a conf\n")

	got, err := Assemble(root)
	require.NoError(t, err)

	assert.NotContains(t, got, "import /homelab", "no bind-mount import globs")
	assert.NotContains(t, got, "conf.d", "legacy dirs are gone")
	assert.NotContains(t, got, "localhost:2019")
	assert.Contains(t, got, "persist_config off")
	assert.Contains(t, got, fmt.Sprintf(`storage file_system %q`, filepath.Join(root, "caddy-data")))
	assert.Contains(t, got, fmt.Sprintf(`admin "unix/%s|0600"`, filepath.Join(root, "caddy-admin.sock")))
	assert.Contains(t, got, "(wildcard_tls) {")
	assert.Contains(t, got, "http://:80 {")
	assert.Contains(t, got, "http://:8081 {")
	assert.NotContains(t, got, "not a conf")

	l4 := strings.Index(got, "layer4 {")
	ssh := strings.Index(got, "hl-ts/homelab:22 {")
	glob := strings.Index(got, "(wildcard_tls)")
	require.True(t, l4 >= 0 && ssh > l4 && ssh < glob, "l4 servers are inlined in the global layer4 block")

	a, b := strings.Index(got, "a.{$HOME_SUBDOMAIN}"), strings.Index(got, "b.{$HOME_SUBDOMAIN}")
	require.True(t, a > glob && b > a, "sites are appended after the snippet, sorted by filename")
}

func TestAssemble_AdminSocketOverride(t *testing.T) {
	t.Setenv(AdminEnv, "/run/hl/admin.sock")
	got, err := Assemble(t.TempDir())
	require.NoError(t, err)
	assert.Contains(t, got, `admin "unix//run/hl/admin.sock|0600"`)
}

// The env placeholders are expanded in memory; Caddy never reads process env.
func TestAdapt_ExpandsPlaceholdersInMemory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOMAIN", "wrong.example")
	writeFile(t, siteFile(root, "a"), "a.{$HOME_SUBDOMAIN}.{$DOMAIN} {\n\trespond {$MISSING:fallback}\n}\n")
	cfg, err := Adapt(root, testEnv)
	require.NoError(t, err)
	assert.Contains(t, string(cfg), "a.home.example.test")
	assert.Contains(t, string(cfg), "fallback")
	assert.Contains(t, string(cfg), testEnv["CLOUDFLARE_API_TOKEN"])
	assert.NotContains(t, string(cfg), "wrong.example")
}

func TestAdapt_BrokenSiteIsInvalidConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, siteFile(root, "bad"), "http://:1 {\n\treverse_proxy {\n")
	_, err := Adapt(root, testEnv)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

// ── Edge ──────────────────────────────────────────────────────────────────────

func TestEdge_LoadsAllServicesOnStart(t *testing.T) {
	root := t.TempDir()
	p1, p2 := freePort(t), freePort(t)
	writeFile(t, siteFile(root, "one"), httpSite(p1, "one"))
	writeFile(t, siteFile(root, "two"), httpSite(p2, "two"))
	e := testEdge(t, root)

	require.NoError(t, e.Start(context.Background()))
	assert.Equal(t, "one", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", p1)))
	assert.Equal(t, "two", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", p2)))

	fi, err := os.Stat(filepath.Join(root, "admin.sock"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "admin socket is owner-only")
}

func TestEdge_InvalidConfigKeepsPrevious(t *testing.T) {
	root := t.TempDir()
	port := freePort(t)
	writeFile(t, siteFile(root, "good"), httpSite(port, "good"))
	e := testEdge(t, root)
	require.NoError(t, e.Start(context.Background()))
	before := caddy.ActiveContext().Context

	writeFile(t, siteFile(root, "broken"), "http://:1 {\n\tno_such_directive\n}\n")
	err := e.Reload()
	require.ErrorIs(t, err, ErrInvalidConfig)
	assert.Equal(t, before, caddy.ActiveContext().Context, "the running config is unchanged")
	assert.Equal(t, "good", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", port)))
}

// A reload starts the new config before stopping the old one on the same
// ports; the registered networks must share their sockets across that.
func TestEdge_ReloadKeepsTailnetListeners(t *testing.T) {
	root := t.TempDir()
	port := freePort(t)
	writeFile(t, siteFile(root, "web"), httpSite(port, "v1"))
	e := testEdge(t, root)
	require.NoError(t, e.Start(context.Background()))
	assert.Equal(t, "v1", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", port)))

	writeFile(t, siteFile(root, "web"), httpSite(port, "v2"))
	require.NoError(t, e.Reload())
	assert.Equal(t, "v2", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", port)))
	require.NoError(t, e.Reload())
	assert.Equal(t, "v2", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", port)))
}

func tcpEcho(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func udpEcho(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(append([]byte("echo:"), buf[:n]...), addr)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func TestEdge_HTTPThroughTailnetBind(t *testing.T) {
	root := t.TempDir()
	port := freePort(t)
	writeFile(t, siteFile(root, "web"), httpSite(port, "via hl-ts"))
	e := testEdge(t, root)
	require.NoError(t, e.Start(context.Background()))
	assert.Equal(t, "via hl-ts", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", port)))
}

// A tailnet site is HTTPS, and Caddy adds an HTTP/3 listener for it: hl-ts
// must map to hl-tsu for QUIC or the whole config fails to start.
func TestEdge_HTTPSSiteOnTailnetBind(t *testing.T) {
	root := t.TempDir()
	port := freePort(t)
	writeFile(t, siteFile(root, "web"), fmt.Sprintf(
		"https://localhost:%d {\n\tbind hl-ts/homelab\n\ttls internal\n\trespond \"secure\"\n}\n", port))
	e := testEdge(t, root, func(s string) string {
		return strings.Replace(s, "auto_https disable_redirects", "auto_https disable_redirects\n\tskip_install_trust", 1)
	})
	require.NoError(t, e.Start(context.Background()))
	// The internal certificate is issued asynchronously after the load.
	var body string
	require.Eventually(t, func() bool {
		var err error
		body, err = get(t, fmt.Sprintf("https://localhost:%d/", port))
		return err == nil
	}, 15*time.Second, 100*time.Millisecond)
	assert.Equal(t, "secure", body)
}

func TestEdge_TCPThroughTailnetL4(t *testing.T) {
	root := t.TempDir()
	port, echo := freePort(t), tcpEcho(t)
	writeFile(t, l4File(root, "echo"), fmt.Sprintf(
		"hl-ts/homelab:%d {\n\troute {\n\t\tproxy 127.0.0.1:%d\n\t}\n}\n", port, echo))
	e := testEdge(t, root)
	require.NoError(t, e.Start(context.Background()))

	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))
}

func TestEdge_UDPThroughTailnetL4(t *testing.T) {
	root := t.TempDir()
	port, echo := freePort(t), udpEcho(t)
	writeFile(t, l4File(root, "dns"), fmt.Sprintf(
		"hl-tsu/homelab:%d {\n\troute {\n\t\tproxy udp/127.0.0.1:%d\n\t}\n}\n", port, echo))
	e := testEdge(t, root)
	require.NoError(t, e.Start(context.Background()))

	c, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Write([]byte("dgram"))
	require.NoError(t, err)
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "echo:dgram", string(buf[:n]))

	// survives a reload
	require.NoError(t, e.Reload())
	_, err = c.Write([]byte("again"))
	require.NoError(t, err)
	n, err = c.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "echo:again", string(buf[:n]))
}

// The CLI reloads the agent's Caddy over the admin socket.
func TestEdge_AdminSocketLoad(t *testing.T) {
	root := t.TempDir()
	p1, p2 := freePort(t), freePort(t)
	writeFile(t, siteFile(root, "one"), httpSite(p1, "one"))
	e := testEdge(t, root)
	require.NoError(t, e.Start(context.Background()))
	sock := AdminSocket(root)

	writeFile(t, siteFile(root, "two"), httpSite(p2, "two"))
	cfg, err := e.adapt()
	require.NoError(t, err)
	require.NoError(t, PostLoad(sock, cfg))
	assert.Equal(t, "two", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", p2)))

	bad := bytes.Replace(cfg, []byte(`"handler":"static_response"`), []byte(`"handler":"no_such_handler"`), 1)
	require.NotEqual(t, cfg, bad)
	err = PostLoad(sock, bad)
	require.ErrorIs(t, err, ErrInvalidConfig)
	assert.Equal(t, "one", mustGet(t, fmt.Sprintf("http://127.0.0.1:%d/", p1)))

	require.NoError(t, e.Stop())
	err = PostLoad(sock, cfg)
	require.ErrorIs(t, err, ErrAgentDown)
	assert.False(t, errors.Is(err, ErrInvalidConfig))
}
