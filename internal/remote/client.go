package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/secrets"
)

// ClusterSecretVar is the root keyring secret holding the cluster secret.
const ClusterSecretVar = "CLUSTER_SECRET"

// Client forwards commands to the leader agent.
type Client struct {
	Endpoint string // https://homelab.<tailnet>.ts.net:7443
	Secret   string // the cluster secret; only its HMAC token is sent
	Version  string // this CLI's version
	// TLSConfig is nil in production: the agent's tsnet certificate is a
	// real Let's Encrypt one, verified against the system roots. Tests set
	// it to trust their own CA.
	TLSConfig *tls.Config
}

// RunOptions is one forwarded invocation.
type RunOptions struct {
	Argv           []string
	TTY            bool // run under a pty; stdout carries both streams
	Rows, Cols     uint16
	Term           string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Resize         <-chan [2]uint16 // terminal size changes (rows, cols)
}

// ClientConfig is client.yaml.
type ClientConfig struct {
	Endpoint string `yaml:"endpoint"`
}

// ClientConfigPath is ${XDG_CONFIG_HOME:-~/.config}/homelab/client.yaml.
func ClientConfigPath() string {
	return filepath.Join(config.DefaultConfigDir(), "client.yaml")
}

// KeyringSecret reads the cluster secret from the local keyring.
func KeyringSecret() (string, error) {
	sm, err := secrets.Open()
	if err != nil {
		return "", err
	}
	return sm.Get("", ClusterSecretVar)
}

// LoadClient builds the client from client.yaml and the cluster secret.
func LoadClient(version string, secret func() (string, error)) (*Client, error) {
	path := ClientConfigPath()
	b, err := os.ReadFile(path) //nolint:gosec // the user's own config
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no homelab agent configured (%s is missing) — run `homelab login` or `homelab join`", path)
	}
	if err != nil {
		return nil, err
	}
	var cc ClientConfig
	if err := yaml.Unmarshal(b, &cc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cc.Endpoint == "" {
		return nil, fmt.Errorf("%s has no endpoint — run `homelab login`", path)
	}
	s, err := secret()
	if err != nil {
		return nil, fmt.Errorf("read the cluster secret from the keyring: %w", err)
	}
	if s == "" {
		return nil, fmt.Errorf("no cluster secret in the local keyring (%s) — run `homelab login`",
			secrets.RootKey(ClusterSecretVar))
	}
	return &Client{Endpoint: cc.Endpoint, Secret: s, Version: version}, nil
}

// Run runs o.Argv on the agent and returns the remote exit code. A non-nil
// error means the command did not run to completion there: the agent
// refused it (the code is then the agent's), or the connection failed.
func (c *Client) Run(ctx context.Context, o RunOptions) (int, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return 1, fmt.Errorf("agent endpoint %q must be an https:// URL", c.Endpoint)
	}
	u.Scheme, u.Path = "wss", Path
	d := websocket.Dialer{TLSClientConfig: c.TLSConfig, HandshakeTimeout: 15 * time.Second}
	conn, resp, err := d.DialContext(ctx, u.String(), http.Header{"Authorization": {"Bearer " + Token(c.Secret)}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return 1, fmt.Errorf("the agent at %s refused this machine's cluster secret (401) — run `homelab login`", u.Host)
		}
		return 1, fmt.Errorf("connect to the agent at %s: %w", u.Host, err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetReadLimit(maxFrame)
	ss := &session{conn: conn}

	hs, _ := json.Marshal(Handshake{Argv: o.Argv, TTY: o.TTY, Rows: o.Rows, Cols: o.Cols, Version: c.Version, Term: o.Term})
	if err := ss.write(websocket.TextMessage, hs); err != nil {
		return 1, fmt.Errorf("send to the agent: %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() { // a cancelled caller closes the connection; the agent then stops the child
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	if o.Stdin != nil {
		go pumpStdin(ss, o.Stdin)
	}
	if o.Resize != nil {
		go func() {
			for {
				select {
				case <-done:
					return
				case r, ok := <-o.Resize:
					if !ok {
						return
					}
					_ = ss.control(control{Resize: &r})
				}
			}
		}()
	}

	for {
		kind, b, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return 1, ctx.Err()
			}
			return 1, fmt.Errorf("connection to the agent lost: %w", err)
		}
		if kind == websocket.BinaryMessage {
			if o.Stdout != nil {
				_, _ = o.Stdout.Write(b)
			}
			continue
		}
		var m control
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if len(m.Stderr) > 0 && o.Stderr != nil {
			_, _ = o.Stderr.Write(m.Stderr)
		}
		if m.Exit != nil {
			ss.mu.Lock()
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			ss.mu.Unlock()
			if m.Error != "" {
				return *m.Exit, errors.New(m.Error)
			}
			return *m.Exit, nil
		}
	}
}

// pumpStdin forwards r until it ends, then tells the agent so.
func pumpStdin(ss *session, r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if ss.write(websocket.BinaryMessage, append([]byte(nil), buf[:n]...)) != nil {
				return
			}
		}
		if err != nil {
			_ = ss.control(control{EOF: true})
			return
		}
	}
}
