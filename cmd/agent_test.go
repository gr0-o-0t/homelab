package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/agent"
)

// fakeCF is a Cloudflare zone holding DNS records, recording every write.
type fakeCF struct {
	mu      sync.Mutex
	records []map[string]any
	writes  []string
	seq     int
}

func (f *fakeCF) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		reply := func(v any) {
			b, _ := json.Marshal(map[string]any{"success": true, "result": v})
			_, _ = w.Write(b)
		}
		const recs = "/zones/z1/dns_records"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			reply([]map[string]string{{"id": "z1"}})
		case r.Method == http.MethodGet && r.URL.Path == recs:
			var out []map[string]any
			for _, rec := range f.records {
				if rec["name"] == r.URL.Query().Get("name") {
					out = append(out, rec)
				}
			}
			reply(out)
		case r.Method == http.MethodPost && r.URL.Path == recs:
			var rec map[string]any
			b, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(b, &rec))
			f.seq++
			rec["id"] = "r" + string(rune('0'+f.seq))
			f.records = append(f.records, rec)
			f.writes = append(f.writes, "POST "+rec["content"].(string))
			reply(rec)
		case r.Method == http.MethodPatch:
			var upd map[string]any
			b, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(b, &upd))
			id := r.URL.Path[len(recs)+1:]
			for _, rec := range f.records {
				if rec["id"] == id {
					for k, v := range upd {
						rec[k] = v
					}
				}
			}
			f.writes = append(f.writes, "PATCH "+id+" "+upd["content"].(string))
			reply(upd)
		case r.Method == http.MethodDelete:
			id := r.URL.Path[len(recs)+1:]
			kept := f.records[:0]
			for _, rec := range f.records {
				if rec["id"] != id {
					kept = append(kept, rec)
				}
			}
			f.records = kept
			f.writes = append(f.writes, "DELETE "+id)
			reply(nil)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeCF) get(i int, k string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[i][k]
}

func (f *fakeCF) set(i int, k string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[i][k] = v
}

func (f *fakeCF) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

func (f *fakeCF) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// The wildcard A record is created once, left alone while it is right, and
// moved when the tailnet IP changes — never proxied.
func TestAgent_WildcardRecordFollowsTailnetIP_Cloudflare(t *testing.T) {
	cf := &fakeCF{}
	srv := cf.server(t)
	const name = "*.home.example.com"

	require.NoError(t, ensureARecord(srv.URL, "tok", "example.com", name, "100.64.0.1"))
	require.NoError(t, ensureARecord(srv.URL, "tok", "example.com", name, "100.64.0.1"))
	assert.Equal(t, []string{"POST 100.64.0.1"}, cf.Writes(), "an unchanged record is not written")
	assert.Equal(t, false, cf.get(0, "proxied"))
	assert.Equal(t, "A", cf.get(0, "type"))

	require.NoError(t, ensureARecord(srv.URL, "tok", "example.com", name, "100.64.0.2"))
	assert.Equal(t, []string{"POST 100.64.0.1", "PATCH r1 100.64.0.2"}, cf.Writes())
	require.Equal(t, 1, cf.count())
	assert.Equal(t, "100.64.0.2", cf.get(0, "content"))

	// A proxied record with the right IP is still wrong.
	cf.set(0, "proxied", true)
	require.NoError(t, ensureARecord(srv.URL, "tok", "example.com", name, "100.64.0.2"))
	assert.Equal(t, false, cf.get(0, "proxied"))
}

func TestEnsureARecord_ExtraARecordsAreRemoved(t *testing.T) {
	cf := &fakeCF{records: []map[string]any{
		{"id": "old", "type": "A", "name": "*.home.example.com", "content": "100.64.0.9", "proxied": false},
		{"id": "good", "type": "A", "name": "*.home.example.com", "content": "100.64.0.1", "proxied": false},
	}}
	srv := cf.server(t)
	require.NoError(t, ensureARecord(srv.URL, "tok", "example.com", "*.home.example.com", "100.64.0.1"))
	assert.Equal(t, []string{"DELETE old"}, cf.Writes())
}

func TestEnsureARecord_OtherTypeIsNotReplaced(t *testing.T) {
	cf := &fakeCF{records: []map[string]any{
		{"id": "c", "type": "CNAME", "name": "*.home.example.com", "content": "elsewhere.example.net"},
	}}
	srv := cf.server(t)
	err := ensureARecord(srv.URL, "tok", "example.com", "*.home.example.com", "100.64.0.1")
	require.ErrorContains(t, err, "a CNAME record for *.home.example.com already exists")
	assert.Empty(t, cf.Writes())
}

func TestNodeInfoFrom(t *testing.T) {
	assert.Equal(t, agent.NodeInfo{Role: agent.RoleLeader, Hostname: "minis"},
		nodeInfoFrom(dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, ControlAvailable: true}, "minis"))
	assert.Equal(t, agent.NodeInfo{Role: agent.RoleWorker, Hostname: "laptop"},
		nodeInfoFrom(dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive}, "laptop"))
	for _, s := range []dswarm.LocalNodeState{dswarm.LocalNodeStateInactive, dswarm.LocalNodeStatePending, dswarm.LocalNodeStateLocked, dswarm.LocalNodeStateError} {
		assert.Equal(t, agent.RoleNone, nodeInfoFrom(dswarm.Info{LocalNodeState: s}, "x").Role, s)
	}
}

// The agent command is hidden plumbing with a --root flag defaulting to the
// state volume, and refused when forwarded.
func TestAgentCommand(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"agent"})
	require.NoError(t, err)
	assert.Equal(t, agentCmd, c)
	assert.True(t, c.Hidden)
	assert.Equal(t, "/var/lib/homelab", c.Flags().Lookup("root").DefValue)
	assert.Equal(t, "/hl", c.Flags().Lookup("shim-dir").DefValue)
	path, ok := ResolveCommand([]string{"agent", "--root", "/x"})
	assert.True(t, ok)
	assert.Equal(t, []string{"agent"}, path)
}
