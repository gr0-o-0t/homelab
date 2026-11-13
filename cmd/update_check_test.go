package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/release"
)

// stubUpdateCheck points the release check at a TLS httptest server that
// serves tag as the latest release (empty: a 500), a leader answering
// leaderVersion (or leaderErr), and the CLI at version. Returns a
// reader of what the bare `update` printed.
func stubUpdateCheck(t *testing.T, version, tag, leaderVersion string, leaderErr error) {
	t.Helper()
	t.Setenv("HOMELAB_AGENT", "")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag == "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","assets":[]}`))
	}))
	prevV, prevC, prevA, prevL := Version, releaseTransport, releaseAPI, updateLeaderVersion
	t.Cleanup(func() {
		srv.Close()
		Version, releaseTransport, releaseAPI, updateLeaderVersion = prevV, prevC, prevA, prevL
		invokedArgs = nil
		resetFlags()
	})
	Version = version
	releaseTransport = trusting(srv)
	releaseAPI = srv.URL
	updateLeaderVersion = func(context.Context) (string, error) { return leaderVersion, leaderErr }
}

func updateOut(t *testing.T, wantErr bool) string {
	t.Helper()
	resetFlags()
	var out bytes.Buffer
	err := runUpdateCheck(context.Background(), &out)
	if wantErr {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	return out.String()
}

func TestUpdateCheck_Available(t *testing.T) {
	stubUpdateCheck(t, "v0.5.0", "v0.5.1", "v0.5.0", nil)
	out := updateOut(t, false)
	assert.Contains(t, out, "homelab CLI v0.5.0 (this PC), agents: leader v0.5.0")
	assert.Contains(t, out, "homelab v0.5.1 is available: run homelab upgrade")
	assert.Contains(t, out, "https://github.com/gr0-o-0t/homelab/releases/tag/v0.5.1")
}

func TestUpdateCheck_UpToDate(t *testing.T) {
	stubUpdateCheck(t, "v0.5.0", "v0.5.0", "v0.5.0", nil)
	assert.Contains(t, updateOut(t, false), "homelab is up to date (v0.5.0)")
}

func TestUpdateCheck_NewerThanLatestIsNotAnUpdate(t *testing.T) {
	stubUpdateCheck(t, "v0.6.0", "v0.5.0", "v0.6.0", nil)
	out := updateOut(t, false)
	assert.Contains(t, out, "up to date")
	assert.NotContains(t, out, "is available")
}

func TestUpdateCheck_DevBuild(t *testing.T) {
	stubUpdateCheck(t, "v0.0.0-dev", "v0.5.1", "v0.5.0", nil)
	out := updateOut(t, false)
	assert.Contains(t, out, "development build")
	assert.Contains(t, out, "the latest release is v0.5.1")
	assert.NotContains(t, out, "is available")
	assert.NotContains(t, out, "up to date")
}

func TestUpdateCheck_LeaderUnreachableContinues(t *testing.T) {
	stubUpdateCheck(t, "v0.5.0", "v0.5.1", "", errors.New("no route"))
	out := updateOut(t, false)
	assert.Contains(t, out, "leader: unreachable (no route)")
	assert.Contains(t, out, "v0.5.1 is available")
}

func TestUpdateCheck_GitHubUnreachableIsAnError(t *testing.T) {
	stubUpdateCheck(t, "v0.5.0", "", "v0.5.0", nil)
	updateOut(t, true)
}

func TestUpdate_BareRunsLocallyServiceForwarded(t *testing.T) {
	stubUpdateCheck(t, "v0.5.0", "v0.5.0", "v0.5.0", nil)
	calls := fakeAgent(t, 0, nil)

	assert.Equal(t, 0, executeArgs([]string{"update"}))
	assert.Empty(t, *calls, "bare update is not forwarded")

	for _, argv := range [][]string{{"update", "web"}, {"update", "-a"}, {"update", "--group", "g"}} {
		resetFlags()
		*calls = nil
		assert.Equal(t, 0, executeArgs(argv), "%q", argv)
		assert.Equal(t, [][]string{argv}, *calls, "%q is forwarded", argv)
	}
}

// slowServer serves a release whose archive's body arrives after delay
// (headers first), and at /releases/slow a release JSON as slowly.
func slowServer(t *testing.T, delay time.Duration) (*httptest.Server, release.Release) {
	t.Helper()
	const tag = "v1.5.0"
	archive := releaseArchive(t, tag)
	var srv *httptest.Server
	slow := func(w http.ResponseWriter, body []byte) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(delay)
		_, _ = w.Write(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/archive", func(w http.ResponseWriter, _ *http.Request) { slow(w, archive) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(archive), release.AssetName(tag))
	})
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		slow(w, []byte(`{"tag_name":"`+tag+`","assets":[]}`))
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	prevT, prevA := releaseTransport, apiTimeout
	t.Cleanup(func() { releaseTransport, apiTimeout = prevT, prevA })
	releaseTransport = trusting(srv)
	return srv, release.Release{Tag: tag, Assets: map[string]string{
		release.AssetName(tag): srv.URL + "/archive",
		release.SumsAsset:      srv.URL + "/sums",
	}}
}

// The download client has no overall timeout (a large archive on a slow
// link takes longer than an API call may); the API client keeps one.
func TestDownloadOutlastsTheAPITimeout(t *testing.T) {
	srv, rel := slowServer(t, 400*time.Millisecond)
	apiTimeout = 100 * time.Millisecond

	bin, err := release.FetchBinary(context.Background(), downloadClient(), rel)
	require.NoError(t, err)
	assert.Equal(t, "NEW-v1.5.0", string(bin))

	_, err = release.Latest(context.Background(), releaseClient(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Client.Timeout")
}

// The download client is bounded by its transport: dial, TLS handshake
// and response headers each time out.
func TestDownloadClientTransportTimeouts(t *testing.T) {
	c := downloadClient()
	assert.Zero(t, c.Timeout)
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, 30*time.Second, tr.TLSHandshakeTimeout)
	assert.Equal(t, 60*time.Second, tr.ResponseHeaderTimeout)
	assert.NotNil(t, tr.DialContext)
	assert.Equal(t, 15*time.Minute, downloadDeadline)
}

// trusting is a base transport that trusts srv's test CA.
func trusting(srv *httptest.Server) func() *http.Transport {
	return func() *http.Transport { return srv.Client().Transport.(*http.Transport).Clone() }
}
