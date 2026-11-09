package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/docker/docker/api/types/container"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

const (
	testIP     = "100.101.102.103"
	testSuffix = "tail1234.ts.net"
	testTSKey  = "tskey-auth-reusable-SECRET"
)

// fakeTailscale answers `tailscale ip -4` and `tailscale status --json`.
func fakeTailscale(ip string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "tailscale" {
			return nil, fmt.Errorf("unexpected command %s", name)
		}
		switch strings.Join(args, " ") {
		case "ip -4":
			return []byte(ip + "\n"), nil
		case "status --json":
			return []byte(`{"MagicDNSSuffix":"` + testSuffix + `","Self":{}}`), nil
		}
		return nil, fmt.Errorf("unexpected tailscale %v", args)
	}
}

func noFile(string) ([]byte, error) { return nil, os.ErrNotExist }

func newKeyring() *secrets.Manager { return secrets.NewForTest(keyring.NewArrayKeyring(nil)) }

type initHarness struct {
	fake    *swarmtest.Fake
	kr      *secrets.Manager
	saved   string
	out     bytes.Buffer
	prompts []string
}

func newInit(t *testing.T, ip string) (*Init, *initHarness) {
	t.Helper()
	h := &initHarness{fake: swarmtest.New(), kr: newKeyring()}
	h.fake.SetInfo(system.Info{Name: "mini1"})
	require.NoError(t, h.kr.Set("", "TS_AUTHKEY", testTSKey))
	in := &Init{
		API:      h.fake,
		Run:      fakeTailscale(ip),
		Keyring:  h.kr,
		ReadFile: noFile,
		Prompt: func(label string, _ bool) (string, error) {
			h.prompts = append(h.prompts, label)
			return "", nil
		},
		SaveClient: func(ep string) error { h.saved = ep; return nil },
		Image:      "ghcr.io/gr0-o-0t/homelab:v1.2.3",
		Out:        &h.out,
	}
	return in, h
}

func TestInit_SwarmInitOnTailnetIP(t *testing.T) {
	in, h := newInit(t, testIP)
	require.NoError(t, in.Do(context.Background()))

	inits := h.fake.Inits()
	require.Len(t, inits, 1)
	assert.Equal(t, testIP, inits[0].AdvertiseAddr)
	assert.Equal(t, testIP, inits[0].DataPathAddr)
	assert.Equal(t, "0.0.0.0:2377", inits[0].ListenAddr)

	nodes := h.fake.Nodes()
	require.Len(t, nodes, 1)
	assert.Equal(t, "core", nodes[0].Spec.Labels["homelab.role"])

	created := h.fake.Created()
	require.Len(t, created, 1)
	c := created[0]
	assert.Equal(t, AgentContainer, c.Name)
	assert.True(t, c.Started)
	assert.Equal(t, "unless-stopped", string(c.HostConfig.RestartPolicy.Name))
	assert.ElementsMatch(t, []string{
		"/var/run/docker.sock:/var/run/docker.sock",
		"homelab-agent:/var/lib/homelab",
		"homelab-shim:/hl",
		"homelab-backups:/var/lib/homelab-backups",
	}, c.HostConfig.Binds)
	ep := c.Networking.EndpointsConfig["home-services"]
	require.NotNil(t, ep, "the leader agent joins home-services")
	assert.Equal(t, []string{"homelab-edge"}, ep.Aliases)
	assert.Contains(t, c.Config.Env, "XDG_DATA_HOME=/var/lib/homelab/xdg/data")
	assert.Contains(t, c.Config.Env, "XDG_CONFIG_HOME=/var/lib/homelab/xdg/config")
	assert.Equal(t, []string{"agent"}, []string(c.Config.Cmd))
	assert.Equal(t, "ghcr.io/gr0-o-0t/homelab:v1.2.3", c.Config.Image)

	// the overlay exists before the agent is attached to it
	log := strings.Join(h.fake.CallLog(), "\n")
	assert.Less(t, strings.Index(log, "NetworkCreate"), strings.Index(log, "ContainerCreate"))

	assert.Equal(t, "https://homelab."+testSuffix+":7443", h.saved)
	secret, err := h.kr.Get("", "CLUSTER_SECRET")
	require.NoError(t, err)
	assert.Len(t, secret, 64)
	assert.Contains(t, h.out.String(), "homelab join")
	assert.Contains(t, h.out.String(), "reusable")
}

func TestInit_RefusesNonTailnetAddress(t *testing.T) {
	for _, ip := range []string{"192.168.1.10", "100.128.0.1", "10.0.0.1"} {
		in, h := newInit(t, ip)
		err := in.Do(context.Background())
		require.Error(t, err, ip)
		assert.Contains(t, err.Error(), "100.64.0.0/10")
		assert.Empty(t, h.fake.Inits(), "no swarm on %s", ip)
	}

	// tailscale absent or down
	in, h := newInit(t, testIP)
	in.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exec: \"tailscale\": executable file not found in $PATH")
	}
	err := in.Do(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tailscale")
	assert.Empty(t, h.fake.Inits())
}

func TestInit_SecretNeverInRunArgs(t *testing.T) {
	in, h := newInit(t, testIP)
	require.NoError(t, h.kr.Set("", "CF_TUNNEL_TOKEN", "cf-tunnel-SECRET"))
	require.NoError(t, in.Do(context.Background()))
	secret, err := h.kr.Get("", "CLUSTER_SECRET")
	require.NoError(t, err)
	require.NotEmpty(t, secret)

	created := h.fake.Created()
	require.Len(t, created, 1)
	spec, err := json.Marshal(created[0])
	require.NoError(t, err)
	for _, s := range []string{secret, testTSKey, "cf-tunnel-SECRET"} {
		assert.NotContains(t, string(spec), s, "the agent container spec carries a secret")
	}

	execs := h.fake.Execs()
	require.Len(t, execs, 1)
	e := execs[0]
	assert.Equal(t, AgentContainer, e.Container)
	argv, _ := json.Marshal(e.Options)
	for _, s := range []string{secret, testTSKey, "cf-tunnel-SECRET"} {
		assert.NotContains(t, string(argv), s, "the seed exec's argv/env carries a secret")
	}
	assert.True(t, e.Options.AttachStdin)
	var seeded map[string]string
	require.NoError(t, json.Unmarshal(e.Stdin, &seeded))
	assert.Equal(t, secret, seeded["CLUSTER_SECRET"])
	assert.Equal(t, testTSKey, seeded["TS_AUTHKEY"])
	assert.Equal(t, "cf-tunnel-SECRET", seeded["CF_TUNNEL_TOKEN"])
	assert.Equal(t, 1, created[0].Restarts, "the agent restarts to read the seeded vault")
}

func TestInit_WarnsAboutUFW(t *testing.T) {
	in, h := newInit(t, testIP)
	in.ReadFile = func(p string) ([]byte, error) {
		if p == "/etc/ufw/ufw.conf" {
			return []byte("# comment\nENABLED=yes\nLOGLEVEL=low\n"), nil
		}
		return nil, os.ErrNotExist
	}
	require.NoError(t, in.Do(context.Background()))
	out := h.out.String()
	for _, l := range []string{
		"sudo ufw allow in on tailscale0 to any port 2377 proto tcp",
		"sudo ufw allow in on tailscale0 to any port 7946 proto tcp",
		"sudo ufw allow in on tailscale0 to any port 7946 proto udp",
		"sudo ufw allow in on tailscale0 to any port 4789 proto udp",
	} {
		assert.Contains(t, out, l)
	}

	// ufw disabled: nothing said
	assert.Empty(t, FirewallHints(func(string) ([]byte, error) { return []byte("ENABLED=no\n"), nil }, true))
	assert.Empty(t, FirewallHints(noFile, true))
	// a worker needs no 2377
	hints := strings.Join(FirewallHints(func(string) ([]byte, error) { return []byte("ENABLED=yes\n"), nil }, false), "\n")
	assert.NotContains(t, hints, "2377")
	assert.Contains(t, hints, "4789")
}

type joinHarness struct {
	fake  *swarmtest.Fake
	kr    *secrets.Manager
	calls [][]string
	saved string
	out   bytes.Buffer
}

func newJoin(t *testing.T) (*Join, *joinHarness) {
	t.Helper()
	h := &joinHarness{fake: swarmtest.New(), kr: newKeyring()}
	h.fake.SetInfo(system.Info{Name: "laptop"})
	j := &Join{
		API:      h.fake,
		Run:      fakeTailscale(testIP),
		Keyring:  h.kr,
		ReadFile: noFile,
		Secret:   "cafe",
		Leader: func(_ context.Context, endpoint, secret string, argv []string) ([]byte, error) {
			if endpoint != "https://homelab."+testSuffix+":7443" || secret != "cafe" {
				return nil, fmt.Errorf("wrong leader %s / %s", endpoint, secret)
			}
			h.calls = append(h.calls, argv)
			if argv[1] == "join-token" {
				return json.Marshal(JoinToken{WorkerToken: "SWMTKN-1-worker", ManagerAddr: "100.64.0.1:2377", TSAuthKey: testTSKey})
			}
			return nil, nil
		},
		SaveClient: func(ep string) error { h.saved = ep; return nil },
		Image:      "ghcr.io/gr0-o-0t/homelab:v1.2.3",
		Out:        &h.out,
	}
	return j, h
}

func TestJoin_LabelsWorker(t *testing.T) {
	j, h := newJoin(t)
	require.NoError(t, j.Do(context.Background()))

	joins := h.fake.Joins()
	require.Len(t, joins, 1)
	assert.Equal(t, testIP, joins[0].AdvertiseAddr)
	assert.Equal(t, testIP, joins[0].DataPathAddr)
	assert.Equal(t, []string{"100.64.0.1:2377"}, joins[0].RemoteAddrs)
	assert.Equal(t, "SWMTKN-1-worker", joins[0].JoinToken)

	info, _ := h.fake.Info(context.Background())
	require.Len(t, h.calls, 2)
	assert.Equal(t, []string{"cluster", "join-token"}, h.calls[0])
	assert.Equal(t, []string{"cluster", "label", info.Swarm.NodeID, "worker"}, h.calls[1])

	created := h.fake.Created()
	require.Len(t, created, 1)
	assert.Empty(t, created[0].Networking.EndpointsConfig, "a worker agent joins no overlay")
	assert.True(t, created[0].Started)

	assert.Equal(t, "https://homelab."+testSuffix+":7443", h.saved)
	s, _ := h.kr.Get("", "CLUSTER_SECRET")
	assert.Equal(t, "cafe", s)
}

func TestJoin_SeedsWorkerVaultWithTSKey(t *testing.T) {
	j, h := newJoin(t)
	require.NoError(t, j.Do(context.Background()))
	execs := h.fake.Execs()
	require.Len(t, execs, 1)
	var seeded map[string]string
	require.NoError(t, json.Unmarshal(execs[0].Stdin, &seeded))
	assert.Equal(t, map[string]string{"TS_AUTHKEY": testTSKey, "CLUSTER_SECRET": "cafe"}, seeded)
	argv := strings.Join(execs[0].Options.Cmd, " ")
	assert.NotContains(t, argv, testTSKey)
	assert.NotContains(t, argv, "cafe")
	assert.Contains(t, argv, "--tailnet "+testSuffix)
	assert.Equal(t, 1, h.fake.Created()[0].Restarts)
}

func TestJoin_RefusesNonTailnetAddress(t *testing.T) {
	j, h := newJoin(t)
	j.Run = fakeTailscale("192.168.0.4")
	require.ErrorContains(t, j.Do(context.Background()), "100.64.0.0/10")
	assert.Empty(t, h.calls)
	assert.Empty(t, h.fake.Joins())
}

func TestAgentSeed_RejectsUnknownNames(t *testing.T) {
	root := t.TempDir()
	_, err := ApplySeed(root, strings.NewReader(`{"TS_AUTHKEY":"k","DOMAIN":"example.com"}`), SeedInfo{})
	require.ErrorContains(t, err, "DOMAIN")
	_, err = os.Stat(filepath.Join(root, "vault.bin"))
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing stored when any name is refused")

	stored, err := ApplySeed(root, strings.NewReader(`{"TS_AUTHKEY":"k","CLUSTER_SECRET":"s"}`),
		SeedInfo{Tailnet: testSuffix, LeaderAddr: "100.64.0.1:2377"})
	require.NoError(t, err)
	assert.Equal(t, []string{"CLUSTER_SECRET", "TS_AUTHKEY"}, stored)
	p := agentstate.At(root)
	sm, err := secrets.OpenVault(p.VaultFile, p.KeyFile)
	require.NoError(t, err)
	c, err := agentstate.LoadCluster(p, sm)
	require.NoError(t, err)
	assert.Equal(t, "s", c.Secret)
	assert.Equal(t, testSuffix, c.Tailnet)
	assert.Equal(t, "100.64.0.1:2377", c.LeaderAddr)
	k, _ := sm.Get("", "TS_AUTHKEY")
	assert.Equal(t, "k", k)
}

func TestIssueJoinToken_RefusedOffManager(t *testing.T) {
	f := swarmtest.New()
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "w1"}})
	_, err := IssueJoinToken(context.Background(), f, newKeyring())
	require.ErrorContains(t, err, "manager")
	require.ErrorContains(t, LabelNode(context.Background(), f, "w1", "worker"), "manager")
}

func TestIssueJoinToken_OnManager(t *testing.T) {
	f := swarmtest.New()
	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "m1",
		ControlAvailable: true, NodeAddr: "100.64.0.1"}})
	f.SetSwarm(dswarm.Swarm{JoinTokens: dswarm.JoinTokens{Worker: "SWMTKN-w", Manager: "SWMTKN-m"}})
	sm := newKeyring()
	require.NoError(t, sm.Set("", "TS_AUTHKEY", testTSKey))
	tok, err := IssueJoinToken(context.Background(), f, sm)
	require.NoError(t, err)
	assert.Equal(t, JoinToken{WorkerToken: "SWMTKN-w", ManagerAddr: "100.64.0.1:2377", TSAuthKey: testTSKey}, tok)

	f.SetNodes([]dswarm.Node{{ID: "w1"}})
	require.NoError(t, LabelNode(context.Background(), f, "w1", "worker"))
	assert.Equal(t, "worker", f.Nodes()[0].Spec.Labels["homelab.role"])
	require.Error(t, LabelNode(context.Background(), f, "w1", "boss"))
}

func TestLocateTask(t *testing.T) {
	f := swarmtest.New()
	ctx := context.Background()
	// not a manager: local
	_, local, err := LocateTask(ctx, f, "gitea", "")
	require.NoError(t, err)
	assert.True(t, local)

	f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive, NodeID: "m1", ControlAvailable: true}})
	f.SetNodes([]dswarm.Node{{ID: "m1", Description: dswarm.NodeDescription{Hostname: "mini1"}},
		{ID: "n2", Description: dswarm.NodeDescription{Hostname: "laptop"}}})
	resp, err := f.ServiceCreate(ctx, dswarm.ServiceSpec{Annotations: dswarm.Annotations{Name: "gitea_gitea",
		Labels: map[string]string{"homelab.service": "gitea"}}}, dswarm.ServiceCreateOptions{})
	require.NoError(t, err)
	f.AddTask(dswarm.Task{ID: "t1", ServiceID: resp.ID, NodeID: "n2", DesiredState: dswarm.TaskStateRunning,
		Status: dswarm.TaskStatus{State: dswarm.TaskStateRunning}})
	host, local, err := LocateTask(ctx, f, "gitea", "")
	require.NoError(t, err)
	assert.False(t, local)
	assert.Equal(t, "laptop", host)
}

func TestInit_RefusesEngineAlreadyInSwarm(t *testing.T) {
	in, h := newInit(t, testIP)
	h.fake.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive}})
	err := in.Do(context.Background())
	require.ErrorContains(t, err, "docker swarm leave --force")
	assert.Empty(t, h.fake.Inits())
	assert.Empty(t, h.fake.Created())
}

func TestInit_RefusesExistingAgentContainer(t *testing.T) {
	in, h := newInit(t, testIP)
	h.fake.SetContainers([]container.Summary{{ID: "c1", Names: []string{"/homelab-agent"}, State: "exited"}})
	err := in.Do(context.Background())
	require.ErrorContains(t, err, "docker rm -f homelab-agent")
	assert.Empty(t, h.fake.Inits())
}

func TestInit_PullFailureTouchesNoSwarm(t *testing.T) {
	in, h := newInit(t, testIP)
	h.fake.SetPullStream(in.Image, `{"status":"Pulling from gr0-o-0t/homelab"}`+"\n"+
		`{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}`+"\n")
	err := in.Do(context.Background())
	require.ErrorContains(t, err, "manifest unknown")
	for _, c := range h.fake.CallLog() {
		assert.False(t, strings.HasPrefix(c, "Swarm") || strings.HasPrefix(c, "Container") && c != "ContainerList",
			"mutation after a failed pull: %s", c)
	}
	assert.Empty(t, h.fake.Inits())
}

func TestEnsureImage_SurfacesStreamErrors(t *testing.T) {
	f := swarmtest.New()
	f.SetPullStream("img:1", `{"errorDetail":{"message":"toomanyrequests: rate limit"}}`+"\n")
	require.ErrorContains(t, EnsureImage(context.Background(), f, "img:1", io.Discard), "toomanyrequests")
	f.SetPullStream("img:2", `{"status":"Downloaded newer image for img:2"}`+"\n")
	require.NoError(t, EnsureImage(context.Background(), f, "img:2", io.Discard))
}

// A failure after SwarmInit leaves the swarm so a retry starts clean.
func TestInit_LeavesSwarmWhenLaterStepFails(t *testing.T) {
	in, h := newInit(t, testIP)
	h.fake.SetExecExitCode(1) // agent-seed fails
	require.Error(t, in.Do(context.Background()))
	assert.Len(t, h.fake.Inits(), 1)
	assert.Equal(t, 1, h.fake.Leaves())
}

func TestResolveImage(t *testing.T) {
	for _, v := range []string{"dev", "", "v0.0.0-20261105150029-969b7619af8a"} {
		_, err := ResolveImage("", v)
		require.ErrorContains(t, err, "--image", v)
	}
	img, err := ResolveImage("", "v1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/gr0-o-0t/homelab:v1.2.3", img)
	img, err = ResolveImage("local/homelab:test", "dev")
	require.NoError(t, err)
	assert.Equal(t, "local/homelab:test", img)
}

func TestJoin_RefusesPlainHTTPEndpointBeforeCallingLeader(t *testing.T) {
	j, h := newJoin(t)
	j.Endpoint = "http://homelab.tail1234.ts.net:7443"
	require.ErrorContains(t, j.Do(context.Background()), "https://")
	assert.Empty(t, h.calls)
}

func TestJoin_RefusesEngineAlreadyInSwarm(t *testing.T) {
	j, h := newJoin(t)
	h.fake.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive}})
	require.ErrorContains(t, j.Do(context.Background()), "docker swarm leave --force")
	assert.Empty(t, h.calls, "the secret is not sent before preflight passes")
	assert.Empty(t, h.fake.Joins())
}

// taskContainer is a running Swarm task container of stack, as translate
// labels it.
func taskContainer(id, stack, key, containerName string, created int64) container.Summary {
	l := map[string]string{SwarmServiceLabel: stack + "_" + key, "com.docker.stack.namespace": stack}
	if containerName != "" {
		l["homelab.container_name"] = containerName
	}
	return container.Summary{ID: id, State: "running", Created: created, Labels: l}
}

func immichContainers() []container.Summary {
	return []container.Summary{
		taskContainer("srv", "immich", "immich-server", "immich", 1),
		taskContainer("ml", "immich", "immich-machine-learning", "immich-machine-learning", 1),
	}
}

func TestTaskContainer(t *testing.T) {
	ctx := context.Background()
	f := swarmtest.New()
	f.SetContainers([]container.Summary{
		taskContainer("old", "gitea", "gitea", "", 1), taskContainer("new", "gitea", "gitea", "", 2),
		taskContainer("db", "gitea", "db", "", 3), taskContainer("x", "other", "other", "", 4),
	})
	id, err := TaskContainer(ctx, f, "gitea", "")
	require.NoError(t, err)
	assert.Equal(t, "new", id, "the newest task during a rolling update")
	id, err = TaskContainer(ctx, f, "gitea", "db")
	require.NoError(t, err)
	assert.Equal(t, "db", id)

	// one service: no key needed
	f.SetContainers([]container.Summary{taskContainer("w", "glance", "web", "", 1)})
	id, err = TaskContainer(ctx, f, "glance", "")
	require.NoError(t, err)
	assert.Equal(t, "w", id)

	_, err = TaskContainer(ctx, f, "nothing", "")
	require.ErrorContains(t, err, "no running container on this node")
}

// immich has no immich_immich: its server is the one whose container_name
// is the stack's name; -c takes a compose key or a container_name.
func TestTaskContainer_ImmichDefaultAndContainerFlag(t *testing.T) {
	ctx := context.Background()
	f := swarmtest.New()
	f.SetContainers(immichContainers())
	for want, id := range map[string]string{
		"": "srv", "immich-machine-learning": "ml", "immich-server": "srv", "immich": "srv",
	} {
		got, err := TaskContainer(ctx, f, "immich", want)
		require.NoError(t, err, want)
		assert.Equal(t, id, got, "-c %q", want)
	}
	_, err := TaskContainer(ctx, f, "immich", "redis")
	require.ErrorContains(t, err, `no container "redis"`)
	require.ErrorContains(t, err, "immich-machine-learning")
}

func TestPickService_AmbiguousStackRefusedWithList(t *testing.T) {
	_, err := PickService("appflowy", []StackService{
		{Name: "appflowy_web", Key: "web", ContainerName: "appflowy-web"},
		{Name: "appflowy_cloud", Key: "cloud", ContainerName: "appflowy-cloud"},
	}, "")
	require.ErrorContains(t, err, "-c")
	require.ErrorContains(t, err, "cloud (appflowy-cloud), web (appflowy-web)")
}

// `foo` must never reach a container of the stack `foo_bar` (whose task
// containers are named foo_bar_<key>): selection is by exact stack label.
func TestTaskContainer_ExactStackLabel(t *testing.T) {
	f := swarmtest.New()
	f.SetContainers([]container.Summary{taskContainer("fb", "foo_bar", "web", "", 1)})
	_, err := TaskContainer(context.Background(), f, "foo", "")
	require.ErrorContains(t, err, "no running container on this node")
	_, err = TaskContainer(context.Background(), f, "foo", "bar_web")
	require.Error(t, err)
}

type infoErrAPI struct{ *swarmtest.Fake }

func (infoErrAPI) Info(context.Context) (system.Info, error) {
	return system.Info{}, errors.New("Cannot connect to the Docker daemon")
}

func TestLocateTask_ReportsEngineErrors(t *testing.T) {
	_, local, err := LocateTask(context.Background(), infoErrAPI{swarmtest.New()}, "gitea", "")
	require.ErrorContains(t, err, "Docker daemon")
	assert.False(t, local)
}

// An engine init must refuse is refused before any secret is asked for.
func TestInit_PreflightBeforeAnyPrompt(t *testing.T) {
	for name, set := range map[string]func(*swarmtest.Fake){
		"in a swarm": func(f *swarmtest.Fake) {
			f.SetInfo(system.Info{Swarm: dswarm.Info{LocalNodeState: dswarm.LocalNodeStateActive}})
		},
		"agent exists": func(f *swarmtest.Fake) {
			f.SetContainers([]container.Summary{{ID: "c", Names: []string{"/homelab-agent"}}})
		},
	} {
		in, h := newInit(t, testIP)
		require.NoError(t, h.kr.Delete("", "TS_AUTHKEY")) // would be prompted for
		set(h.fake)
		require.Error(t, in.Do(context.Background()), name)
		assert.Empty(t, h.prompts, "%s: prompted before refusing", name)
	}
}

// Backups live in their own volume on the leader, never inside the agent's
// state volume; workers have no backups to hold.
func TestAgentSpec_LeaderMountsBackupsVolume(t *testing.T) {
	_, hc, _ := AgentSpec("img", true)
	assert.Contains(t, hc.Binds, "homelab-backups:/var/lib/homelab-backups")
	for _, b := range hc.Binds {
		_, target, _ := strings.Cut(b, ":")
		assert.False(t, strings.HasPrefix(target, agentstate.Root+"/"), "nothing mounted inside the state volume: %s", b)
	}

	_, hc, _ = AgentSpec("img", false)
	for _, b := range hc.Binds {
		assert.NotContains(t, b, "homelab-backups", "workers do not mount the backups volume")
	}
}
