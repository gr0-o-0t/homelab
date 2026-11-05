package caddy

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/edge"
)

var reloadEnv = map[string]string{
	"DOMAIN":               "example.test",
	"HOME_SUBDOMAIN":       "home",
	"ACME_EMAIL":           "admin@example.test",
	"CLOUDFLARE_API_TOKEN": "0123456789abcdefghijklmnopqrstuvwxyzABCD",
}

type posted struct {
	method, path, host, ctype, cache string
	body                             string
}

// fakeAdmin serves Caddy's admin API on a unix socket, answering every request
// with status, and records what it was sent.
func fakeAdmin(t *testing.T, sock string, status int) <-chan posted {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	got := make(chan posted, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // test
		b, _ := io.ReadAll(r.Body)
		got <- posted{r.Method, r.URL.Path, r.Host, r.Header.Get("Content-Type"), r.Header.Get("Cache-Control"), string(b)}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"loading new config: unknown module"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return got
}

func reloadRoot(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	writeSite(t, root, "gitea", "gitea.{$HOME_SUBDOMAIN}.{$DOMAIN} {\n\tbind hl-ts/homelab\n\timport wildcard_tls\n\treverse_proxy gitea:3000\n}\n")
	sock := filepath.Join(root, "admin.sock")
	t.Setenv(edge.AdminEnv, sock)
	return root, sock
}

func writeSite(t *testing.T, root, svc, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(configgen.SitesDir(root), 0o750))
	require.NoError(t, os.WriteFile(configgen.SitesFile(root, svc), []byte(body), 0o600))
}

func liveManager(root string) *Manager {
	return &Manager{RepoRoot: root, envFn: func() (map[string]string, error) { return reloadEnv, nil }}
}

func TestManagerReload_PostsToAdminSocket(t *testing.T) {
	root, sock := reloadRoot(t)
	got := fakeAdmin(t, sock, http.StatusOK)

	require.NoError(t, liveManager(root).Reload())
	p := <-got
	assert.Equal(t, http.MethodPost, p.method)
	assert.Equal(t, "/load", p.path)
	assert.Equal(t, "application/json", p.ctype)
	assert.Equal(t, "must-revalidate", p.cache, "forced, like caddy reload --force")
	assert.Contains(t, p.body, "gitea.home.example.test", "placeholders expanded, sites inlined")
	assert.Contains(t, p.body, `unix/`+sock, "the loaded config keeps the admin socket")
	assert.NotContains(t, p.body, "{$")
}

func TestManagerReload_RejectedIsInvalidConfig(t *testing.T) {
	root, sock := reloadRoot(t)
	fakeAdmin(t, sock, http.StatusBadRequest)
	err := liveManager(root).Reload()
	require.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "unknown module")
}

func TestManagerReload_AdaptErrorIsInvalidConfig(t *testing.T) {
	root, sock := reloadRoot(t)
	got := fakeAdmin(t, sock, http.StatusOK)
	writeSite(t, root, "broken", "http://:1 {\n\tno_such_directive\n}\n")
	require.ErrorIs(t, liveManager(root).Reload(), ErrInvalidConfig)
	assert.Empty(t, got, "nothing is posted when the config does not adapt")
}

func TestManagerReload_AgentDownIsNotInvalidConfig(t *testing.T) {
	root, _ := reloadRoot(t) // no server on the socket
	// Even a broken config is not blamed while the agent is down.
	writeSite(t, root, "broken", "http://:1 {\n\tno_such_directive\n}\n")
	m := liveManager(root)
	m.envFn = func() (map[string]string, error) { t.Fatal("env (keyring) read with the agent down"); return nil, nil }
	err := m.Reload()
	require.Error(t, err)
	assert.ErrorIs(t, err, edge.ErrAgentDown)
	assert.False(t, errors.Is(err, ErrInvalidConfig), "a down agent must not roll the change back")
}
