package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/swarm"
)

// init, join, login and logout set this machine up and run here, never on
// an agent (remote.LocalOnly).

// hostRun runs host commands (tailscale); a var for tests.
var hostRun bootstrap.Runner = bootstrap.ExecRunner

// saveClient writes client.yaml; a var for tests.
var saveClient = remote.SaveClientConfig

var initFlags struct {
	image, importDir string
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create the cluster on this machine (first mini): Swarm manager and the leader agent",
	Long: `Create the cluster on this machine, which becomes the Swarm manager and runs
the leader agent (Caddy, Tailscale, the remote CLI).

Needs Docker Engine and Tailscale up on the host: Swarm advertises the host's
tailnet address only. TS_AUTHKEY (a reusable key) and the Cloudflare tokens are
taken from this machine's keyring (homelab setup stored them) or prompted for.
It prints the command that adds the other PCs.`,
	Args: cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		if initFlags.importDir != "" {
			return errors.New("--import is not implemented yet")
		}
		img, err := bootstrap.ResolveImage(initFlags.image, Version)
		if err != nil {
			return err
		}
		sm, err := openSecrets()
		if err != nil {
			return fmt.Errorf("opening the keyring: %w", err)
		}
		return withSwarmAPI(func(api swarm.API) error {
			in := &bootstrap.Init{
				API:        api,
				Run:        hostRun,
				Keyring:    sm,
				Prompt:     terminalPrompt,
				ReadFile:   os.ReadFile,
				SaveClient: saveClient,
				Image:      img,
				Out:        c.OutOrStdout(),
			}
			return in.Do(cmdContext())
		})
	},
}

var joinFlags struct {
	image, endpoint string
	secretStdin     bool
}

var joinCmd = &cobra.Command{
	Use:   "join",
	Short: "Add this machine to the cluster as a worker",
	Long: `Add this machine to the cluster as a Swarm worker running its own agent, and
make it a client of the leader. Asks for the cluster secret that homelab init
printed (or reads it from stdin with --secret-stdin).

Needs Docker Engine and Tailscale up on the host.`,
	Args: cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		// Refuse a bad endpoint or image before the secret is asked for.
		if joinFlags.endpoint != "" {
			if err := bootstrap.ValidateEndpoint(joinFlags.endpoint); err != nil {
				return err
			}
		}
		img, err := bootstrap.ResolveImage(joinFlags.image, Version)
		if err != nil {
			return err
		}
		secret, err := readClusterSecret(os.Stdin, joinFlags.secretStdin)
		if err != nil {
			return err
		}
		sm, err := openSecrets()
		if err != nil {
			return fmt.Errorf("opening the keyring: %w", err)
		}
		return withSwarmAPI(func(api swarm.API) error {
			j := &bootstrap.Join{
				API:        api,
				Run:        hostRun,
				Keyring:    sm,
				Secret:     secret,
				Endpoint:   joinFlags.endpoint,
				Leader:     callLeader,
				ReadFile:   os.ReadFile,
				SaveClient: saveClient,
				Image:      img,
				Out:        c.OutOrStdout(),
			}
			return j.Do(cmdContext())
		})
	},
}

func init() {
	initCmd.Flags().StringVar(&initFlags.image, "image", "", "agent image (default "+bootstrap.ImageRepo+":<version>; required on a development build)")
	initCmd.Flags().StringVar(&initFlags.importDir, "import", "", "import an existing single-host config dir")
	joinCmd.Flags().StringVar(&joinFlags.image, "image", "", "agent image (default "+bootstrap.ImageRepo+":<version>; required on a development build)")
	joinCmd.Flags().StringVar(&joinFlags.endpoint, "endpoint", "", "the leader's remote CLI URL (default https://homelab.<tailnet>:7443)")
	joinCmd.Flags().BoolVar(&joinFlags.secretStdin, "secret-stdin", false, "read the cluster secret from stdin")
	rootCmd.AddCommand(initCmd, joinCmd)
}

// terminalPrompt asks on the terminal; secrets are not echoed.
func terminalPrompt(label string, secret bool) (string, error) {
	if secret {
		return promptSecret(label, false), nil
	}
	fmt.Printf("  %s: ", label)
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text()), nil
	}
	return "", sc.Err()
}

// readClusterSecret reads the first line of r with fromStdin, else prompts.
func readClusterSecret(r io.Reader, fromStdin bool) (string, error) {
	var s string
	if fromStdin {
		sc := bufio.NewScanner(r)
		if sc.Scan() {
			s = sc.Text()
		}
		if err := sc.Err(); err != nil {
			return "", err
		}
	} else {
		s = promptSecret("Cluster secret (printed by homelab init)", false)
	}
	if s = strings.TrimSpace(s); s == "" {
		return "", errors.New("no cluster secret given")
	}
	return s, nil
}

// callLeader runs argv on the leader agent and returns its stdout.
func callLeader(ctx context.Context, endpoint, secret string, argv []string) ([]byte, error) {
	c := &remote.Client{Endpoint: endpoint, Secret: secret, Version: Version}
	var out bytes.Buffer
	code, err := c.Run(ctx, remote.RunOptions{Argv: argv, Stdout: &out, Stderr: os.Stderr})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("`homelab %s` on the leader exited %d", strings.Join(argv, " "), code)
	}
	return out.Bytes(), nil
}
