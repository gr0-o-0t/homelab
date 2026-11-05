package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/remote"
)

var loginFlags struct {
	endpoint    string
	secretStdin bool
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Make this machine a client of the cluster (no agent, no swarm)",
	Long: `Make this machine a client of the cluster: every command then runs on the
leader agent. Asks for the leader's endpoint and the cluster secret; the
endpoint goes to client.yaml, the secret to this machine's keyring.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return login(os.Stdin, loginFlags.secretStdin, loginFlags.endpoint)
	},
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Forget the cluster endpoint and secret on this machine",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if err := logout(); err != nil {
			return err
		}
		fmt.Println("Logged out: client.yaml and the cluster secret removed.")
		return nil
	},
}

func init() {
	loginCmd.Flags().StringVar(&loginFlags.endpoint, "endpoint", "", "the leader's remote CLI URL (default https://homelab.<tailnet>:7443)")
	loginCmd.Flags().BoolVar(&loginFlags.secretStdin, "secret-stdin", false, "read the cluster secret from stdin")
	rootCmd.AddCommand(loginCmd, logoutCmd)
}

// login stores the endpoint (asked for when empty, offering the tailnet's
// default) and the cluster secret (from stdin's first line, or asked for).
func login(stdin io.Reader, secretStdin bool, endpoint string) error {
	// One reader for both: the endpoint line must not swallow the secret's.
	br := bufio.NewReader(stdin)
	if endpoint == "" {
		def := ""
		if suffix, err := bootstrap.TailnetSuffix(cmdContext(), hostRun); err == nil {
			def = bootstrap.LeaderEndpoint(suffix)
		}
		if def != "" {
			fmt.Printf("  Leader endpoint [%s]: ", def)
		} else {
			fmt.Print("  Leader endpoint (https://homelab.<tailnet>.ts.net:7443): ")
		}
		line, _ := br.ReadString('\n')
		if endpoint = strings.TrimSpace(line); endpoint == "" {
			endpoint = def
		}
	}
	if u, err := url.Parse(endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("endpoint %q must be an https:// URL", endpoint)
	}
	secret, err := readClusterSecret(br, secretStdin)
	if err != nil {
		return err
	}
	sm, err := openSecrets()
	if err != nil {
		return fmt.Errorf("opening the keyring: %w", err)
	}
	if err := sm.Set("", remote.ClusterSecretVar, secret); err != nil {
		return fmt.Errorf("storing the cluster secret: %w", err)
	}
	return saveClient(endpoint)
}

// logout removes client.yaml and the cluster secret.
func logout() error {
	sm, err := openSecrets()
	if err != nil {
		return fmt.Errorf("opening the keyring: %w", err)
	}
	if err := sm.Delete("", remote.ClusterSecretVar); err != nil {
		return fmt.Errorf("removing the cluster secret: %w", err)
	}
	return remote.RemoveClientConfig()
}
