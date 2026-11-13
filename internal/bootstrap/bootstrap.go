// Package bootstrap creates and joins the cluster: `homelab init` on the
// first mini (Swarm manager, the leader agent), `homelab join` on every other
// PC (Swarm worker, a worker agent), and the leader's side of a join.
//
// Swarm control (2377), gossip (7946) and the VXLAN data path (4789) ride the
// tailnet only: both commands advertise the host's Tailscale IPv4 and refuse
// any address outside 100.64.0.0/10. Secrets reach the agent's vault through
// the stdin of a one-shot exec in its container, never through argv, env,
// labels or the container spec.
package bootstrap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
)

const (
	// AgentContainer is the agent's container name on every PC.
	AgentContainer = "homelab-agent"
	// ImageRepo is the agent image; the tag is the CLI's version.
	ImageRepo = "ghcr.io/gr0-o-0t/homelab"
	// EdgeAlias is the leader agent's name on home-services: Caddy's
	// upstream for every service reaches it there.
	EdgeAlias = "homelab-edge"
	// ManagerPort is Swarm's control-plane port.
	ManagerPort = "2377"
	// RemotePort is the agents' remote CLI port on the tailnet.
	RemotePort = "7443"
	// ShimVolume holds the agent binary Swarm containers run as entrypoint.
	ShimVolume = "homelab-shim"
	// StateVolume is the agent's state (vault, cluster.yaml, state dir).
	StateVolume = "homelab-agent"

	ufwConf = "/etc/ufw/ufw.conf"
)

// SeedNames are the only names agent-seed stores: root secrets. Vars
// (DOMAIN, HOME_SUBDOMAIN, ACME_EMAIL) belong in config.yaml, not the vault.
var SeedNames = []string{"CLUSTER_SECRET", "TS_AUTHKEY", "CLOUDFLARE_API_TOKEN", "CF_TUNNEL_TOKEN"}

// tailnetPrefix is Tailscale's CGNAT range.
var tailnetPrefix = netip.MustParsePrefix("100.64.0.0/10")

// DefaultImage is the agent image matching this CLI's version.
func DefaultImage(version string) string { return ImageRepo + ":" + version }

// Runner runs a host command and returns its stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // fixed commands (tailscale)
}

// TailnetIPv4 is the host's Tailscale IPv4 (`tailscale ip -4`). It refuses
// an address outside 100.64.0.0/10, and a host without Tailscale up.
func TailnetIPv4(ctx context.Context, run Runner) (netip.Addr, error) {
	out, err := run(ctx, "tailscale", "ip", "-4")
	if err != nil {
		return netip.Addr{}, fmt.Errorf("finding this host's tailnet address (`tailscale ip -4`): %w — "+
			"install Tailscale on the host and run `tailscale up` first", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	ip, err := netip.ParseAddr(strings.TrimSpace(line))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("`tailscale ip -4` printed %q, not an IPv4 address — is Tailscale up?", line)
	}
	if !ip.Is4() || !tailnetPrefix.Contains(ip) {
		return netip.Addr{}, fmt.Errorf("refusing %s: Swarm must advertise a tailnet address (100.64.0.0/10) "+
			"so its control and data traffic only rides WireGuard", ip)
	}
	return ip, nil
}

// TailnetSuffix is the tailnet's MagicDNS suffix (tail1234.ts.net).
func TailnetSuffix(ctx context.Context, run Runner) (string, error) {
	out, err := run(ctx, "tailscale", "status", "--json")
	if err != nil {
		return "", fmt.Errorf("reading the tailnet name (`tailscale status --json`): %w", err)
	}
	var st struct{ MagicDNSSuffix string }
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("parsing `tailscale status --json`: %w", err)
	}
	s := strings.Trim(st.MagicDNSSuffix, ".")
	if s == "" {
		return "", errors.New("the tailnet has no MagicDNS name — enable MagicDNS in the Tailscale admin console")
	}
	return s, nil
}

// LeaderEndpoint is the leader agent's remote CLI URL.
func LeaderEndpoint(suffix string) string {
	return "https://homelab." + suffix + ":" + RemotePort
}

// NodeEndpoint is a worker agent's remote CLI URL: its tailnet node is
// homelab-<host>.
func NodeEndpoint(hostname, suffix string) string {
	return "https://homelab-" + hostname + "." + suffix + ":" + RemotePort
}

// FirewallHints are the ufw rules to run when ufw is enabled (best effort:
// /etc/ufw/ufw.conf says ENABLED=yes); nil otherwise. Nothing is changed.
func FirewallHints(readFile func(string) ([]byte, error), manager bool) []string {
	b, err := readFile(ufwConf)
	if err != nil {
		return nil
	}
	enabled := false
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(k) == "ENABLED" && strings.EqualFold(strings.Trim(strings.TrimSpace(v), `"'`), "yes") {
			enabled = true
		}
	}
	if !enabled {
		return nil
	}
	var out []string
	if manager {
		out = append(out, "sudo ufw allow in on tailscale0 to any port 2377 proto tcp")
	}
	return append(out,
		"sudo ufw allow in on tailscale0 to any port 7946 proto tcp",
		"sudo ufw allow in on tailscale0 to any port 7946 proto udp",
		"sudo ufw allow in on tailscale0 to any port 4789 proto udp")
}

func printFirewallHints(w io.Writer, readFile func(string) ([]byte, error), manager bool) {
	hints := FirewallHints(readFile, manager)
	if len(hints) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nwarning: ufw is enabled. If it does not already let Swarm through on the tailnet, run:")
	for _, h := range hints {
		_, _ = fmt.Fprintln(w, "  "+h)
	}
}

// AgentSpec is the agent container: the docker socket, its state and shim
// volumes, XDG dirs inside the state volume. The leader's also mounts the
// backups volume and joins home-services as homelab-edge. No secret appears
// anywhere in it.
func AgentSpec(img string, leader bool) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	cfg := &container.Config{
		Image: img,
		Cmd:   []string{"agent"},
		Env: []string{
			"XDG_DATA_HOME=" + agentstate.Root + "/xdg/data",
			"XDG_CONFIG_HOME=" + agentstate.Root + "/xdg/config",
		},
	}
	hc := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		Binds: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			StateVolume + ":" + agentstate.Root,
			ShimVolume + ":/hl",
		},
	}
	nc := &network.NetworkingConfig{}
	if leader {
		// Backups run on the leader (where the data volumes are pinned) and
		// land in their own volume, which `backup --fetch` streams from.
		hc.Binds = append(hc.Binds, agentstate.BackupsVolume+":"+agentstate.BackupsRoot)
		nc.EndpointsConfig = map[string]*network.EndpointSettings{
			swarm.SharedNetwork: {Aliases: []string{EdgeAlias}},
		}
	}
	return cfg, hc, nc
}

// StartAgent creates and starts the agent container. The image must already
// be on the engine (EnsureImage, in preflight).
func StartAgent(ctx context.Context, api swarm.API, img string, leader bool) error {
	cfg, hc, nc := AgentSpec(img, leader)
	if _, err := api.ContainerCreate(ctx, cfg, hc, nc, nil, AgentContainer); err != nil {
		if errdefs.IsConflict(err) {
			return fmt.Errorf("a %s container already exists — remove it and its state (`docker rm -f %s && docker volume rm %s`; data volumes are untouched) and retry: %w",
				AgentContainer, AgentContainer, StateVolume, err)
		}
		return fmt.Errorf("creating the %s container: %w", AgentContainer, err)
	}
	if err := api.ContainerStart(ctx, AgentContainer, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting the %s container: %w", AgentContainer, err)
	}
	return nil
}

// SeedInfo is the non-secret cluster.yaml content agent-seed writes along
// with CLUSTER_SECRET; it travels as flags.
type SeedInfo struct {
	Tailnet    string
	LeaderAddr string
}

func (i SeedInfo) args() []string {
	var a []string
	if i.Tailnet != "" {
		a = append(a, "--tailnet", i.Tailnet)
	}
	if i.LeaderAddr != "" {
		a = append(a, "--leader-addr", i.LeaderAddr)
	}
	return a
}

// SeedAgent stores values in the running agent's vault — `homelab
// agent-seed` exec'd in its container, the values as JSON on stdin — then
// restarts the agent so it reads them (it loads its secrets at start).
func SeedAgent(ctx context.Context, api swarm.API, values map[string]string, info SeedInfo) error {
	if err := seedExec(ctx, api, seedCmd(info), values); err != nil {
		return err
	}
	return restartAgent(ctx, api)
}

// seedCmd is the plain agent-seed command line: no secret in it.
func seedCmd(info SeedInfo) []string {
	return append([]string{"homelab", "agent-seed", "--root", agentstate.Root}, info.args()...)
}

// restartAgent restarts the agent so it reads what was seeded.
func restartAgent(ctx context.Context, api swarm.API) error {
	if err := api.ContainerRestart(ctx, AgentContainer, container.StopOptions{}); err != nil {
		return fmt.Errorf("restarting the agent after seeding its vault: %w", err)
	}
	return nil
}

// seedExec runs cmd in the agent container with values, as JSON, on its
// stdin — the only way secrets travel into the container.
func seedExec(ctx context.Context, api swarm.API, cmd []string, values any) error {
	payload, err := json.Marshal(values)
	if err != nil {
		return err
	}
	ex, err := api.ContainerExecCreate(ctx, AgentContainer, container.ExecOptions{
		Cmd: cmd, AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("seeding the agent's vault: %w", err)
	}
	att, err := api.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("seeding the agent's vault: %w", err)
	}
	defer att.Close()
	if _, err := att.Conn.Write(payload); err != nil {
		return fmt.Errorf("seeding the agent's vault: %w", err)
	}
	if err := att.CloseWrite(); err != nil {
		return fmt.Errorf("seeding the agent's vault: %w", err)
	}
	var output bytes.Buffer
	if _, err := stdcopy.StdCopy(&output, &output, att.Reader); err != nil {
		return fmt.Errorf("seeding the agent's vault: %w", err)
	}
	res, err := api.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		return fmt.Errorf("seeding the agent's vault: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("seeding the agent's vault failed (exit %d): %s", res.ExitCode, strings.TrimSpace(output.String()))
	}
	return nil
}

// maxSeed bounds agent-seed's stdin.
const maxSeed = 1 << 20

// ApplySeed is `homelab agent-seed`: it reads {"NAME":"value",…} from in and
// stores each as a root secret in the vault under root. A name outside
// SeedNames refuses the whole payload before anything is stored. With
// CLUSTER_SECRET it also writes cluster.yaml (info). Returns the names
// stored, sorted.
func ApplySeed(root string, in io.Reader, info SeedInfo) ([]string, error) {
	b, err := io.ReadAll(io.LimitReader(in, maxSeed+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSeed {
		return nil, errors.New("agent-seed: input too large")
	}
	var values map[string]string
	if err := json.Unmarshal(b, &values); err != nil {
		return nil, fmt.Errorf("agent-seed: stdin must be a JSON object of strings: %w", err)
	}
	names := make([]string, 0, len(values))
	for k, v := range values {
		if !slices.Contains(SeedNames, k) {
			return nil, fmt.Errorf("agent-seed: refusing %q — only %s are seeded", k, strings.Join(SeedNames, ", "))
		}
		if v == "" {
			return nil, fmt.Errorf("agent-seed: %s is empty", k)
		}
		names = append(names, k)
	}
	sort.Strings(names)
	p := agentstate.At(root)
	sm, err := secrets.OpenVault(p.VaultFile, p.KeyFile)
	if err != nil {
		return nil, err
	}
	for _, k := range names {
		if k == "CLUSTER_SECRET" {
			continue // SaveCluster stores it, with cluster.yaml
		}
		if err := sm.Set("", k, values[k]); err != nil {
			return nil, fmt.Errorf("agent-seed: storing %s: %w", k, err)
		}
	}
	if s, ok := values["CLUSTER_SECRET"]; ok {
		c := readClusterFile(p.ClusterFile)
		c.Secret = s
		if info.Tailnet != "" {
			c.Tailnet = info.Tailnet
		}
		if info.LeaderAddr != "" {
			c.LeaderAddr = info.LeaderAddr
		}
		if err := agentstate.SaveCluster(p, sm, c); err != nil {
			return nil, fmt.Errorf("agent-seed: storing the cluster settings: %w", err)
		}
	}
	return names, nil
}

// readClusterFile is cluster.yaml's settings, zero when absent or unreadable.
func readClusterFile(path string) agentstate.Cluster {
	var c agentstate.Cluster
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // the agent's own state file
		_ = yaml.Unmarshal(b, &c)
	}
	return c
}

// NewClusterSecret is 32 random bytes, hex.
func NewClusterSecret(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// requireManager is the engine's info, or an error when it is not a Swarm
// manager: the cluster commands run on the leader only.
func requireManager(ctx context.Context, api swarm.API) (system.Info, error) {
	info, err := api.Info(ctx)
	if err != nil {
		return system.Info{}, err
	}
	if info.Swarm.LocalNodeState != dswarm.LocalNodeStateActive || !info.Swarm.ControlAvailable {
		return system.Info{}, errors.New("this node is not a Swarm manager: `homelab cluster` runs on the leader only")
	}
	return info, nil
}

// LabelNode sets the homelab.role label (core or worker) on a node. It runs
// on a manager only.
func LabelNode(ctx context.Context, api swarm.API, nodeID, role string) error {
	if _, err := requireManager(ctx, api); err != nil {
		return err
	}
	if role != string(swarm.RoleCore) && role != string(swarm.RoleWorker) {
		return fmt.Errorf("role %q: must be %s or %s", role, swarm.RoleCore, swarm.RoleWorker)
	}
	return labelNode(ctx, api, nodeID, role)
}

func labelNode(ctx context.Context, api swarm.API, nodeID, role string) error {
	nodes, err := api.NodeList(ctx, dswarm.NodeListOptions{})
	if err != nil {
		return err
	}
	for i := range nodes {
		n := &nodes[i]
		if n.ID != nodeID {
			continue
		}
		spec := n.Spec
		labels := make(map[string]string, len(spec.Labels)+1)
		for k, v := range spec.Labels {
			labels[k] = v
		}
		labels[swarm.LabelNodeRole] = role
		spec.Labels = labels
		if err := api.NodeUpdate(ctx, n.ID, n.Version, spec); err != nil {
			return fmt.Errorf("labelling node %s %s=%s: %w", nodeID, swarm.LabelNodeRole, role, err)
		}
		return nil
	}
	return fmt.Errorf("no swarm node %q", nodeID)
}

// JoinToken is what the leader hands a joining PC (`cluster join-token`).
type JoinToken struct {
	WorkerToken string `json:"worker_token"`
	ManagerAddr string `json:"manager_addr"`
	// TSAuthKey is the leader vault's root:TS_AUTHKEY, for the worker
	// agent's own tailnet node. It must be a reusable key.
	TSAuthKey string `json:"ts_authkey"`
}

// IssueJoinToken is the leader side of `homelab join`.
func IssueJoinToken(ctx context.Context, api swarm.API, sm *secrets.Manager) (JoinToken, error) {
	info, err := requireManager(ctx, api)
	if err != nil {
		return JoinToken{}, err
	}
	sw, err := api.SwarmInspect(ctx)
	if err != nil {
		return JoinToken{}, err
	}
	addr := ""
	for _, m := range info.Swarm.RemoteManagers {
		if m.NodeID == info.Swarm.NodeID {
			addr = m.Addr
		}
	}
	if addr == "" && info.Swarm.NodeAddr != "" {
		addr = net.JoinHostPort(info.Swarm.NodeAddr, ManagerPort)
	}
	if addr == "" {
		return JoinToken{}, errors.New("cannot tell this manager's address")
	}
	key, err := sm.Get("", "TS_AUTHKEY")
	if err != nil {
		return JoinToken{}, fmt.Errorf("reading TS_AUTHKEY from the vault: %w", err)
	}
	if key == "" {
		return JoinToken{}, errors.New("no TS_AUTHKEY in the leader's vault — a worker agent needs one for its tailnet node")
	}
	return JoinToken{WorkerToken: sw.JoinTokens.Worker, ManagerAddr: addr, TSAuthKey: key}, nil
}

// Prompt asks for a value; secret ones are not echoed.
type Prompt func(label string, secret bool) (string, error)

// LeaderCall runs argv on the leader agent at endpoint and returns stdout.
type LeaderCall func(ctx context.Context, endpoint, secret string, argv []string) ([]byte, error)

// Init is `homelab init`.
type Init struct {
	API        swarm.API
	Run        Runner
	Keyring    *secrets.Manager // this PC's keyring: TS/Cloudflare secrets from `homelab setup`, CLUSTER_SECRET
	Prompt     Prompt
	ReadFile   func(string) ([]byte, error)
	SaveClient func(endpoint string) error
	Image      string
	Import     string // --import: a Compose-era config dir to bring in
	TempDir    string // where the import tree is built; "" = os.TempDir
	Out        io.Writer
	Rand       io.Reader
}

// Do creates the cluster on this PC.
func (in *Init) Do(ctx context.Context) (err error) {
	if in.Image == "" {
		return errors.New("no agent image")
	}
	ip, err := TailnetIPv4(ctx, in.Run)
	if err != nil {
		return err
	}
	suffix, err := TailnetSuffix(ctx, in.Run)
	if err != nil {
		return err
	}
	// Refuse an engine that cannot take a new swarm before asking for any
	// secret; the (slow) pull comes after the prompts.
	if err := checkEngine(ctx, in.API); err != nil {
		return err
	}
	// An import is built (and refused) before anything changes: the
	// Compose-era network must be idle, the old dir importable.
	var imp *builtImport
	if in.Import != "" {
		if err := refuseLeftoverStateVolume(ctx, in.API); err != nil {
			return err
		}
		if err := composeContainers(ctx, in.API, in.Import); err != nil {
			return err
		}
		if imp, err = buildImport(in.Import, in.TempDir, in.Keyring); err != nil {
			return err
		}
		defer imp.remove()
	}
	// The Compose-era network must be idle, import or not.
	if _, err := composeBridge(ctx, in.API); err != nil {
		return err
	}
	vals, err := in.secrets()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(in.Out, "Note: TS_AUTHKEY must be a reusable auth key — every `homelab join` hands it to that PC's agent; a one-shot key cannot be reused.")
	if err := EnsureImage(ctx, in.API, in.Image, in.Out); err != nil {
		return err
	}

	limit := swarm.TaskHistoryLimit
	nodeID, err := in.API.SwarmInit(ctx, dswarm.InitRequest{
		ListenAddr:    "0.0.0.0:" + ManagerPort,
		AdvertiseAddr: ip.String(),
		DataPathAddr:  ip.String(),
		Spec:          dswarm.Spec{Orchestration: dswarm.OrchestrationConfig{TaskHistoryRetentionLimit: &limit}},
	})
	if err != nil {
		return fmt.Errorf("creating the swarm on %s: %w", ip, err)
	}
	defer leaveOnError(ctx, in.API, in.Out, &err)
	_, _ = fmt.Fprintf(in.Out, "Swarm created; this node (%s) is the manager, advertising %s.\n", nodeID, ip)
	if err := labelNode(ctx, in.API, nodeID, string(swarm.RoleCore)); err != nil {
		return err
	}
	if err := removeComposeBridge(ctx, in.API); err != nil {
		return err
	}
	if err := (&swarm.Deployer{API: in.API}).EnsureNetwork(ctx); err != nil {
		return err
	}
	// Whether StartAgent creates the state volume or reuses one: the import
	// rollback only removes a volume this run created.
	volumeExisted := false
	if imp != nil {
		if volumeExisted, err = stateVolumeExists(ctx, in.API); err != nil {
			return err
		}
	}
	if err := StartAgent(ctx, in.API, in.Image, true); err != nil {
		return err
	}
	// Until the agent has restarted on the imported state, a failure takes
	// the agent and its state volume away again: otherwise every retry is
	// refused because the volume already has services.
	restarted := false
	if imp != nil {
		defer func() {
			if err != nil && !restarted {
				removeAgentAndState(context.WithoutCancel(ctx), in.API, in.Out, !volumeExisted)
			}
		}()
	}
	secret, err := NewClusterSecret(in.Rand)
	if err != nil {
		return err
	}
	vals["CLUSTER_SECRET"] = secret
	info := SeedInfo{Tailnet: suffix, LeaderAddr: net.JoinHostPort(ip.String(), ManagerPort)}
	if imp == nil {
		err = SeedAgent(ctx, in.API, vals, info)
	} else {
		// Both seeds land before the one restart: the agent brings the
		// imported services up from desired.yaml when it starts.
		err = seedExec(ctx, in.API, seedCmd(info), vals)
		if err == nil {
			err = imp.install(ctx, in.API)
		}
		if err == nil {
			err = restartAgent(ctx, in.API)
		}
		restarted = err == nil
	}
	if err != nil {
		return err
	}
	endpoint := LeaderEndpoint(suffix)
	if err := in.Keyring.Set("", "CLUSTER_SECRET", secret); err != nil {
		return fmt.Errorf("storing the cluster secret in this machine's keyring: %w", err)
	}
	if err := in.SaveClient(endpoint); err != nil {
		return err
	}
	printFirewallHints(in.Out, in.ReadFile, true)
	_, _ = fmt.Fprintf(in.Out, "\nThe homelab agent is starting; its remote CLI will answer at %s.\n", endpoint)
	_, _ = fmt.Fprintf(in.Out, "To add a PC, run on it:\n\n  homelab join --endpoint %s\n\n", endpoint)
	_, _ = fmt.Fprintf(in.Out, "and enter the cluster secret (also in this machine's keyring as %s):\n\n  %s\n",
		secrets.RootKey("CLUSTER_SECRET"), secret)
	if imp != nil {
		printImportReport(in.Out, imp.report)
	}
	return nil
}

// secrets are the root secrets the leader's vault gets from this PC's
// keyring, prompting for what is missing: TS_AUTHKEY is required, the
// Cloudflare tokens optional.
func (in *Init) secrets() (map[string]string, error) {
	vals := map[string]string{}
	for _, k := range []struct {
		name, label string
		required    bool
	}{
		{"TS_AUTHKEY", "Tailscale auth key (TS_AUTHKEY, reusable)", true},
		{"CLOUDFLARE_API_TOKEN", "Cloudflare API token (CLOUDFLARE_API_TOKEN, Enter to skip)", false},
		{"CF_TUNNEL_TOKEN", "Cloudflare Tunnel token (CF_TUNNEL_TOKEN, Enter to skip)", false},
	} {
		v, err := in.Keyring.Get("", k.name)
		if err != nil {
			return nil, fmt.Errorf("reading %s from the keyring: %w", k.name, err)
		}
		if v == "" {
			if v, err = in.Prompt(k.label, true); err != nil {
				return nil, err
			}
		}
		v = strings.TrimSpace(v)
		if v == "" && k.required {
			return nil, fmt.Errorf("%s is required: the leader agent's tailnet node needs it", k.name)
		}
		if v != "" {
			vals[k.name] = v
		}
	}
	return vals, nil
}

// Join is `homelab join`.
type Join struct {
	API        swarm.API
	Run        Runner
	Keyring    *secrets.Manager
	Secret     string // the cluster secret
	Endpoint   string // the leader's; default LeaderEndpoint(suffix)
	Leader     LeaderCall
	ReadFile   func(string) ([]byte, error)
	SaveClient func(endpoint string) error
	Image      string
	Out        io.Writer
}

// Do joins this PC to the cluster as a worker.
func (j *Join) Do(ctx context.Context) (err error) {
	if strings.TrimSpace(j.Secret) == "" {
		return errors.New("no cluster secret given")
	}
	if j.Endpoint != "" {
		if err := ValidateEndpoint(j.Endpoint); err != nil {
			return err
		}
	}
	if j.Image == "" {
		return errors.New("no agent image")
	}
	ip, err := TailnetIPv4(ctx, j.Run)
	if err != nil {
		return err
	}
	suffix, err := TailnetSuffix(ctx, j.Run)
	if err != nil {
		return err
	}
	endpoint := j.Endpoint
	if endpoint == "" {
		endpoint = LeaderEndpoint(suffix)
	}
	if err := preflight(ctx, j.API, j.Image, j.Out); err != nil {
		return err
	}
	// A Compose-era PC keeps a local home-services bridge; the overlay's
	// tasks here would attach to it and be rejected.
	if err := removeComposeBridge(ctx, j.API); err != nil {
		return err
	}
	out, err := j.Leader(ctx, endpoint, j.Secret, []string{"cluster", "join-token"})
	if err != nil {
		return fmt.Errorf("asking the leader for a join token: %w", err)
	}
	var tok JoinToken
	if err := json.NewDecoder(bufio.NewReader(bytes.NewReader(out))).Decode(&tok); err != nil {
		return fmt.Errorf("the leader's join token is not JSON: %w", err)
	}
	if tok.WorkerToken == "" || tok.ManagerAddr == "" || tok.TSAuthKey == "" {
		return errors.New("the leader's join token is incomplete")
	}
	if err := j.API.SwarmJoin(ctx, dswarm.JoinRequest{
		ListenAddr:    "0.0.0.0:" + ManagerPort,
		AdvertiseAddr: ip.String(),
		DataPathAddr:  ip.String(),
		RemoteAddrs:   []string{tok.ManagerAddr},
		JoinToken:     tok.WorkerToken,
	}); err != nil {
		return fmt.Errorf("joining the swarm at %s: %w", tok.ManagerAddr, err)
	}
	defer leaveOnError(ctx, j.API, j.Out, &err)
	info, err := j.API.Info(ctx)
	if err != nil {
		return err
	}
	if info.Swarm.NodeID == "" {
		return errors.New("joined the swarm but the engine reports no node ID")
	}
	if _, err := j.Leader(ctx, endpoint, j.Secret, []string{"cluster", "label", info.Swarm.NodeID, string(swarm.RoleWorker)}); err != nil {
		return fmt.Errorf("asking the leader to label this node a worker: %w", err)
	}
	if err := StartAgent(ctx, j.API, j.Image, false); err != nil {
		return err
	}
	if err := SeedAgent(ctx, j.API, map[string]string{"TS_AUTHKEY": tok.TSAuthKey, "CLUSTER_SECRET": j.Secret},
		SeedInfo{Tailnet: suffix, LeaderAddr: tok.ManagerAddr}); err != nil {
		return err
	}
	if err := j.Keyring.Set("", "CLUSTER_SECRET", j.Secret); err != nil {
		return fmt.Errorf("storing the cluster secret in this machine's keyring: %w", err)
	}
	if err := j.SaveClient(endpoint); err != nil {
		return err
	}
	printFirewallHints(j.Out, j.ReadFile, false)
	_, _ = fmt.Fprintf(j.Out, "Joined the cluster as a worker (node %s, %s).\n", info.Swarm.NodeID, ip)
	_, _ = fmt.Fprintf(j.Out, "The agent's tailnet node is homelab-%s; if it does not come up, Tailscale may have "+
		"rejected the auth key (it must be reusable) — see `docker logs %s`.\n", info.Name, AgentContainer)
	return nil
}
