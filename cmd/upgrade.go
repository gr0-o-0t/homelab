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
	"github.com/spf13/pflag"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/release"
	"github.com/groot/homelab/internal/remote"
)

var upgradeFlags struct {
	version string
	image   string
	cliOnly bool
}

// upgradeCmd upgrades this PC's CLI and the cluster's agents. On a PC it
// finds the target release (the latest, or --version), replaces this
// binary with it (selfupdate.go), then forwards `upgrade --image` to the
// leader, waits for the leader's agent to come back and checks its
// version; on the leader (inside the agent) it does the agents' rollout.
var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade this PC's homelab CLI to the latest release, then the cluster's agents: workers first, the leader last",
	Long: `Replaces this PC's homelab binary with the latest release's (or --version's),
verified against the release's SHA256SUMS; a binary you cannot write is
replaced through sudo (or doas). Then pulls the agent image on every node, replaces
each worker's homelab-agent container (one by one, each back on the new
version before the next) and the leader's last. Mounts, networks, env,
labels and restart policy are kept; no volume is touched. An offline worker
is skipped; rerun upgrade once it is back. --image rolls
that image to the agents only and leaves the CLI alone. An agent already on
the target, the leader included, is left as it is. A running homelab GUI
or dashboard keeps the old version until it is restarted.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if upgradeFlags.cliOnly {
			if os.Getenv("HOMELAB_AGENT") == "1" {
				return errors.New("--cli-only replaces a PC's homelab binary: it does not run in the agent")
			}
			return runCLIOnly(cmdContext(), os.Stdout)
		}
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
		u := &bootstrap.Upgrade{API: api, Image: img, Version: upgradeFlags.version, Current: Version, Call: call, Out: os.Stdout}
		_, err = u.Do(ctx)
		return err
	},
}

func init() {
	upgradeCmd.Flags().StringVar(&upgradeFlags.version, "version", "", "homelab release to upgrade to (default: the latest)")
	upgradeCmd.Flags().StringVar(&upgradeFlags.image, "image", "",
		"agent image to roll out to the agents only, the CLI left as it is (default "+bootstrap.ImageRepo+":<version>)")
	upgradeCmd.Flags().BoolVar(&upgradeFlags.cliOnly, "cli-only", false, "replace this binary with --version's and exit (the step run under sudo)")
	_ = upgradeCmd.Flags().MarkHidden("cli-only")
	rootCmd.AddCommand(upgradeCmd)
}

// upgradeRunsLocally reports whether c is the hidden `upgrade --cli-only`:
// it replaces this PC's binary, so it is never forwarded.
func upgradeRunsLocally(c *cobra.Command) bool {
	return c == upgradeCmd && upgradeFlags.cliOnly
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

// forwardUpgrade is `homelab upgrade` on a PC. --image goes to the agents
// only, as given. Otherwise the target is --version's release or the
// latest: this CLI is replaced with it unless it already is it (a
// development build needs --version: it cannot be compared with a
// release), then the agents are rolled to its image. The leader leaves
// alone every agent already on it, itself included, so a bare upgrade with
// no newer release still catches up agents behind this CLI.
func forwardUpgrade(argv []string) (int, error) {
	if upgradeFlags.image != "" {
		return rollAgents(argv, upgradeFlags.image, upgradeFlags.version, Version)
	}
	// The binary exec'd after the sudo step: it must be the target, and it
	// never runs a CLI step again (no sudo → exec → sudo loop).
	if want := os.Getenv(upgradeSelfEnv); want != "" {
		if Version != want || upgradeFlags.version != want {
			return 1, fmt.Errorf("the installed binary reports %s, not %s; not retrying", Version, want)
		}
		return rollAgents(argv, bootstrap.DefaultImage(want), want, want)
	}
	ctx := cmdContext()
	release.UserAgent = "homelab/" + Version
	tag := upgradeFlags.version
	var rel release.Release
	switch {
	case tag == "":
		if reason, dev := release.DevBuild(Version); dev {
			return 1, fmt.Errorf("%s: it cannot be compared with a release — pass --version <tag> to install a release, or --image <ref> to roll an image to the agents only", reason)
		}
		latest, err := release.Latest(ctx, releaseClient(), releaseAPI)
		if err != nil {
			return 1, fmt.Errorf("finding the latest release: %w", err)
		}
		if !release.Newer(Version, latest.Tag) {
			return upgradeAgentsOnly(ctx, argv, latest.Tag)
		}
		tag, rel = latest.Tag, latest
	case !semverRelease(tag):
		return 1, fmt.Errorf("--version %q is not a release tag (vX.Y.Z); --image <ref> rolls another image to the agents", tag)
	case tag != Version:
		r, err := release.ByTag(ctx, releaseClient(), releaseAPI, tag)
		if err != nil {
			return 1, fmt.Errorf("finding release %s: %w", tag, err)
		}
		rel = r
	}
	if rel.Tag != "" {
		execd, err := upgradeSelf(ctx, rel, changedRootFlags(), os.Stdout)
		switch {
		case err != nil:
			return 1, err
		case execd:
			return 0, nil // only a test's execSelf returns: this process is the new binary's
		}
	}
	return rollAgents(argv, bootstrap.DefaultImage(tag), tag, tag)
}

// semverRelease: tag can name a published release (no development build).
func semverRelease(tag string) bool {
	_, dev := release.DevBuild(tag)
	return !dev
}

// upgradeAgentsOnly is a bare upgrade with no newer release than this CLI:
// the CLI stays, and the agents are rolled to its version — the leader
// leaves alone every agent already on it, so a worker that missed an
// upgrade is caught up. A leader on a newer release than this CLI is never
// downgraded.
func upgradeAgentsOnly(ctx context.Context, argv []string, latest string) (int, error) {
	_, _ = fmt.Fprintf(os.Stdout, "this CLI is already %s (the latest release is %s)\n", Version, latest)
	if c, err := upgradeClient(); err == nil {
		if id, err := upgradeAsk(ctx, c); err == nil && release.Newer(Version, id.Version) {
			_, _ = fmt.Fprintf(os.Stdout, "the leader agent runs %s, newer than this CLI: nothing rolled (install the %s CLI)\n", id.Version, id.Version)
			return 0, nil
		}
	}
	return rollAgents(argv, bootstrap.DefaultImage(Version), Version, Version)
}

// changedRootFlags is the root flags this invocation set, wherever they
// stood in argv (`homelab --no-color upgrade`, `homelab upgrade --no-color`),
// for the exec of the new binary; --config-dir/--config are left out (the
// CLI refuses them in a forwarded argv).
func changedRootFlags() []string {
	var out []string
	rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Changed && f.Name != "config-dir" && f.Name != "config" {
			out = append(out, "--"+f.Name+"="+f.Value.String())
		}
	})
	return out
}

// rollAgents forwards `upgrade --image img [--version want]` to the leader
// (it would otherwise default to its own, older version), and once its
// agent has replaced itself waits for a new agent and checks its version.
// cli is this PC's CLI version, as it is now.
func rollAgents(argv []string, img, want, cli string) (int, error) {
	fwd := append(rootFlagsOf(argv), "upgrade", "--image", img)
	if want != "" {
		fwd = append(fwd, versionFlag, want)
	}
	c, err := upgradeClient()
	if err != nil {
		// `sudo homelab upgrade`: root has no client.yaml or keyring of the
		// user's (and the swap already happened, as root).
		if os.Getenv("SUDO_USER") != "" {
			err = fmt.Errorf("%w — run `homelab upgrade` as yourself, not with sudo: it asks for root only to replace the binary", err)
		}
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
	return verifyLeader(ctx, c, before, want, cli, os.Stderr)
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
func verifyLeader(ctx context.Context, c *remote.Client, before bootstrap.AgentID, want, cli string, w io.Writer) (int, error) {
	_, _ = fmt.Fprintf(w, "agent restarting — reconnecting to the leader (up to %s) …\n", bootstrap.WaitBack)
	got, err := bootstrap.WaitReplaced(ctx, func(ctx context.Context) (bootstrap.AgentID, error) { return upgradeAsk(ctx, c) },
		before, want, upgradeSleep)
	if err != nil {
		return 1, fmt.Errorf("the leader: %w", err)
	}
	_, _ = fmt.Fprintf(w, "The leader agent runs %s.\n", got.Version)
	if got.Version != cli {
		_, _ = fmt.Fprintf(w, "note: this CLI is %s — install the %s CLI on your PCs to match.\n", cli, got.Version)
	}
	return 0, nil
}
