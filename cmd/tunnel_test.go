package cmd

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/docker/docker/api/types/image"
	dswarm "github.com/docker/docker/api/types/swarm"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm"
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

// `cf route add` used `cloudflared tunnel route dns`, which needs the cert.pem
// of an interactive `cloudflared tunnel login` — never present with a token
// tunnel, so it always failed. It now creates the CNAME through the API.
func TestEnsureTunnelCNAME(t *testing.T) {
	newServer := func(existing string, created *map[string]any) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/zones":
				_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"z1"}]}`))
			case r.Method == http.MethodGet && r.URL.Path == "/zones/z1/dns_records":
				assert.Equal(t, "searxng.example.com", r.URL.Query().Get("name"))
				_, _ = w.Write([]byte(`{"success":true,"result":[` + existing + `]}`))
			case r.Method == http.MethodPost && r.URL.Path == "/zones/z1/dns_records":
				require.NoError(t, json.NewDecoder(r.Body).Decode(created))
				_, _ = w.Write([]byte(`{"success":true,"result":{"id":"new"}}`))
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL)
			}
		}))
	}

	var rec map[string]any
	srv := newServer("", &rec)
	created, err := ensureTunnelCNAME(srv.URL, "tok", "example.com", "searxng.example.com", "abc-123")
	srv.Close()
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "CNAME", rec["type"])
	assert.Equal(t, "searxng.example.com", rec["name"])
	assert.Equal(t, "abc-123.cfargotunnel.com", rec["content"])
	assert.Equal(t, true, rec["proxied"], "unproxied would expose the tunnel host instead of routing through it")

	srv = newServer(`{"type":"CNAME","content":"abc-123.cfargotunnel.com"}`, &rec)
	created, err = ensureTunnelCNAME(srv.URL, "tok", "example.com", "searxng.example.com", "abc-123")
	srv.Close()
	require.NoError(t, err)
	assert.False(t, created, "an existing tunnel record is left alone")

	srv = newServer(`{"type":"A","content":"203.0.113.7"}`, &rec)
	_, err = ensureTunnelCNAME(srv.URL, "tok", "example.com", "searxng.example.com", "abc-123")
	srv.Close()
	assert.ErrorContains(t, err, "not replacing", "a record we did not create is never overwritten")
}

func TestTunnelIDFromToken(t *testing.T) {
	tok := base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"6f2c-tunnel","s":"c2VjcmV0"}`))
	id, err := tunnelIDFromToken(tok)
	require.NoError(t, err)
	assert.Equal(t, "6f2c-tunnel", id)

	_, err = tunnelIDFromToken("not a token")
	assert.Error(t, err)
	_, err = tunnelIDFromToken(base64.StdEncoding.EncodeToString([]byte(`{"a":"acct"}`)))
	assert.Error(t, err)
}

// cloudflared is a Swarm service: start deploys it with the token from the
// environment, stop removes it, and no token is an error, not a no-op.
func TestStartStopCloudflared_UsesSwarm(t *testing.T) {
	f := fakeSwarm(t)
	f.SetImage(swarm.CloudflaredImage, image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{Entrypoint: []string{"cloudflared", "--no-autoupdate"}},
	}})
	root := t.TempDir()

	require.Error(t, startCloudflared(root, map[string]string{}))
	require.NoError(t, startCloudflared(root, map[string]string{"CF_TUNNEL_TOKEN": "tok"}))
	svcs, err := f.ServiceList(t.Context(), dswarm.ServiceListOptions{})
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.Equal(t, swarm.CloudflaredService, svcs[0].Spec.Name)

	require.NoError(t, stopCloudflared(root))
	svcs, err = f.ServiceList(t.Context(), dswarm.ServiceListOptions{})
	require.NoError(t, err)
	assert.Empty(t, svcs)
}

// A working tunnel reads as running everywhere: the cf layer, `ext` status,
// containerStatus and doctor take cloudflared's state from its Swarm task,
// not from a container named "cloudflared".
func TestCloudflaredState_FromSwarmTask(t *testing.T) {
	f := fakeSwarm(t)
	f.SetImage(swarm.CloudflaredImage, image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{Entrypoint: []string{"cloudflared"}},
	}})
	root := t.TempDir()
	assert.Equal(t, "not found", cloudflaredState(root))

	require.NoError(t, startCloudflared(root, map[string]string{"CF_TUNNEL_TOKEN": "tok"}))
	svcs, err := f.ServiceList(t.Context(), dswarm.ServiceListOptions{})
	require.NoError(t, err)
	f.SetNodes([]dswarm.Node{{ID: "n1", Description: dswarm.NodeDescription{Hostname: "minis"}}})
	f.AddTask(dswarm.Task{ID: "t1", ServiceID: svcs[0].ID, NodeID: "n1",
		DesiredState: dswarm.TaskStateRunning, Status: dswarm.TaskStatus{State: dswarm.TaskStateRunning}})

	assert.Equal(t, "running", cloudflaredState(root))
	assert.Equal(t, "running", containerStatus(swarm.CloudflaredService))

	l, ok := extRegistry().Get("cf")
	require.True(t, ok)
	assert.Equal(t, swarm.CloudflaredService, l.ContainerName())
	assert.Equal(t, "running", l.Status().ContainerState)
	assert.Equal(t, "running", extLayerState(l, nil))
	assert.Empty(t, l.Profile(), "no compose profile may start a second connector")

	require.NoError(t, stopCloudflared(root))
	f.SetTasks(nil)
	assert.Equal(t, "not found", extLayerState(l, nil))
}
