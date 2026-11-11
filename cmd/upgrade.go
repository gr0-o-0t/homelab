package cmd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/remote"
)

var upgradeFlags struct {
	version string
	image   string
}

// upgradeCmd rolls a new agent image across the cluster. On a PC it resolves
// the image (default: this CLI's own version) and forwards `upgrade --image`
// to the leader, then waits for the leader's agent to come back and checks
// its version; on the leader (inside the agent) it does the rollout.
var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Roll a new homelab version across the cluster's agents: workers first, the leader last",
	Long: `Pulls the agent image on every node, then replaces each worker's homelab-agent
container (one by one, each back on the new version before the next) and the
leader's last. Mounts, networks, env, labels and restart policy are kept;
no volume is touched. An offline worker is skipped; rerun upgrade once it is
back. The default image is the one matching this CLI's version.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		img, err := bootstrap.ResolveImage(upgradeFlags.image, cmp.Or(upgradeFlags.version, Version))
		if err != nil {
			return err
		}
		api, err := newSwarmAPI()
		if err != nil {
			return fmt.Errorf("connecting to docker: %w", err)
		}
		if c, ok := api.(io.Closer); ok {
			defer func() { _ = c.Close() }()
		}
		ctx := cmdContext()
		info, err := api.Info(ctx)
		if err != nil {
			return fmt.Errorf("asking Docker for this node's swarm role: %w", err)
		}
		if !info.Swarm.ControlAvailable {
			return errors.New("upgrade runs on the leader (the swarm manager)")
		}
		call, err := workerCaller()
		if err != nil {
			return err
		}
		u := &bootstrap.Upgrade{API: api, Image: img, Version: upgradeFlags.version, Call: call, Out: os.Stdout}
		_, err = u.Do(ctx)
		return err
	},
}

func init() {
	upgradeCmd.Flags().StringVar(&upgradeFlags.version, "version", "", "homelab version to roll out (default: this CLI's)")
	upgradeCmd.Flags().StringVar(&upgradeFlags.image, "image", "", "agent image to roll out (default "+bootstrap.ImageRepo+":<version>)")
	rootCmd.AddCommand(upgradeCmd)
}

// workerCaller runs a command on a worker's agent over the tailnet (through
// the leader's tailnet proxy, as the exec/logs relay does) and returns its
// stdout.
func workerCaller() (bootstrap.AgentCall, error) {
	cl, err := relayCluster()
	if err != nil {
		return nil, fmt.Errorf("reading the cluster settings: %w", err)
	}
	if cl.Secret == "" || cl.Tailnet == "" {
		return nil, errors.New("the cluster secret or tailnet name is missing from the agent's state — rerun `homelab init`")
	}
	return newWorkerCall(func(host string) string { return bootstrap.NodeEndpoint(host, cl.Tailnet) }, cl.Secret, nil), nil
}

// newWorkerCall is the AgentCall to the agents at endpoint(host): stdout on
// success, the exit status and output otherwise. tlsConfig is nil in
// production (tests trust their own CA).
func newWorkerCall(endpoint func(host string) string, secret string, tlsConfig *tls.Config) bootstrap.AgentCall {
	return func(ctx context.Context, host string, argv []string) (string, error) {
		c := &remote.Client{Endpoint: endpoint(host), Secret: secret, Version: Version, TLSConfig: tlsConfig}
		var out, errb bytes.Buffer
		code, err := c.Run(ctx, remote.RunOptions{Argv: argv, Stdout: &out, Stderr: &errb})
		if err != nil {
			return out.String(), err
		}
		if code != 0 {
			msg := strings.TrimSpace(errb.String() + out.String())
			return out.String(), fmt.Errorf("exit status %d: %s", code, msg)
		}
		return out.String(), nil
	}
}

// upgradeRun runs argv on the leader, its output to stdout; a var so tests
// stand in for it.
var upgradeRun = runOnAgentTo

// upgradeAsk asks the leader its version and boot ID; a var for tests.
var upgradeAsk = func(ctx context.Context, c *remote.Client) (bootstrap.AgentID, error) {
	var out bytes.Buffer
	code, err := c.Run(ctx, remote.RunOptions{Argv: bootstrap.VersionArgv, Stdout: &out, Stderr: io.Discard})
	if err == nil && code != 0 {
		err = fmt.Errorf("exit status %d", code)
	}
	return bootstrap.ParseAgentID(out.String()), err
}

// upgradeClient is the client of the leader agent; a var for tests.
var upgradeClient = func() (*remote.Client, error) { return remote.LoadClient(Version, remote.KeyringSecret) }

// upgradeSleep waits between reconnection attempts; a var for tests.
var upgradeSleep = bootstrap.Sleep

// forwardUpgrade is `homelab upgrade` on a PC: the leader gets the resolved
// image (it would otherwise default to its own, older version), and once
// its agent has replaced itself this waits for a new agent and checks its
// version.
func forwardUpgrade(argv []string) (int, error) {
	img, err := bootstrap.ResolveImage(upgradeFlags.image, cmp.Or(upgradeFlags.version, Version))
	if err != nil {
		return 1, err
	}
	want := upgradeFlags.version
	if want == "" && upgradeFlags.image == "" {
		want = Version
	}
	fwd := append(rootFlagsOf(argv), "upgrade", "--image", img)
	if want != "" {
		fwd = append(fwd, "--version", want)
	}
	c, err := upgradeClient()
	if err != nil {
		return 1, err
	}
	ctx := cmdContext()
	// Who answers now: only a different agent container afterwards is the
	// replacement. An agent that cannot tell leaves only the version check.
	before, err := upgradeAsk(ctx, c)
	if err != nil {
		before = bootstrap.AgentID{}
	}
	mw := &markerWriter{w: os.Stdout, marker: []byte(bootstrap.LeaderReplaceMarker)}
	code, err := upgradeRun(c, fwd, mw)
	switch {
	case code == 130 && err == nil:
		return code, nil // interrupted here; it goes on on the leader
	case !mw.seen:
		if errors.Is(err, remote.ErrConnectionLost) {
			return code, fmt.Errorf("%w — the upgrade continues on the leader (it is detached); follow it with `homelab logs`", err)
		}
		return code, err
	case err == nil && code == 0, errors.Is(err, remote.ErrConnectionLost):
	default:
		return code, err
	}
	return verifyLeader(ctx, c, before, want, os.Stderr)
}

// markerWriter passes output through and notes whether marker went by
// (across writes).
type markerWriter struct {
	w      io.Writer
	marker []byte
	tail   []byte
	seen   bool
}

func (m *markerWriter) Write(p []byte) (int, error) {
	if !m.seen {
		m.tail = append(m.tail, p...)
		if bytes.Contains(m.tail, m.marker) {
			m.seen, m.tail = true, nil
		} else if keep := len(m.marker); len(m.tail) > keep {
			m.tail = append([]byte(nil), m.tail[len(m.tail)-keep:]...)
		}
	}
	return m.w.Write(p)
}

// rootFlagsOf is the root flags before the command name (--no-color).
func rootFlagsOf(argv []string) []string {
	var out []string
	for _, a := range forwardableArgv(argv) {
		if !strings.HasPrefix(a, "-") {
			break
		}
		out = append(out, a)
	}
	return out
}

// verifyLeader waits for a new leader agent (bootstrap.WaitReplaced) and
// reports its version: matching want (when known), or why not.
func verifyLeader(ctx context.Context, c *remote.Client, before bootstrap.AgentID, want string, w io.Writer) (int, error) {
	_, _ = fmt.Fprintf(w, "agent restarting — reconnecting to the leader (up to %s) …\n", bootstrap.WaitBack)
	got, err := bootstrap.WaitReplaced(ctx, func(ctx context.Context) (bootstrap.AgentID, error) { return upgradeAsk(ctx, c) },
		before, want, upgradeSleep)
	if err != nil {
		return 1, fmt.Errorf("the leader: %w", err)
	}
	_, _ = fmt.Fprintf(w, "The leader agent runs %s.\n", got.Version)
	if got.Version != Version {
		_, _ = fmt.Fprintf(w, "note: this CLI is %s — install the %s CLI on your PCs to match.\n", Version, got.Version)
	}
	return 0, nil
}
