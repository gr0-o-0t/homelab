package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `cf route rm` used to run `cloudflared tunnel route dns --overwrite-dns`,
// which re-creates the CNAME, and then printed "removed". It now deletes the
// record through the API — and only the tunnel CNAME, never a hand-made one.
func TestDeleteTunnelCNAME_DeletesOnlyTunnelRecords(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			assert.Equal(t, "example.com", r.URL.Query().Get("name"))
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"z1"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/zones/z1/dns_records":
			assert.Equal(t, "git.example.com", r.URL.Query().Get("name"))
			_, _ = w.Write([]byte(`{"success":true,"result":[
				{"id":"r1","content":"abc.cfargotunnel.com"},
				{"id":"r2","content":"elsewhere.example.net"}]}`))
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"r1"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()

	n, err := deleteTunnelCNAME(srv.URL, "tok", "example.com", "git.example.com")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"/zones/z1/dns_records/r1"}, deleted)
}

func TestDeleteTunnelCNAME_FailsLoudly(t *testing.T) {
	_, err := deleteTunnelCNAME("http://unused", "", "example.com", "git.example.com")
	assert.Error(t, err, "no token is a failure, not a silent success")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zones" {
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"z1"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
	}))
	defer srv.Close()
	_, err = deleteTunnelCNAME(srv.URL, "tok", "example.com", "git.example.com")
	assert.Error(t, err, "nothing deleted must not report success")
}
