package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	prevV, prevC, prevA, prevL := Version, releaseClient, releaseAPI, updateLeaderVersion
	t.Cleanup(func() {
		srv.Close()
		Version, releaseClient, releaseAPI, updateLeaderVersion = prevV, prevC, prevA, prevL
		invokedArgs = nil
		resetFlags()
	})
	Version = version
	releaseClient = srv.Client
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
