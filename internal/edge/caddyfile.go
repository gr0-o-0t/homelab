package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig"

	"github.com/groot/homelab/assets"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/secrets"
)

// ErrInvalidConfig marks a config Caddy rejected — it did not adapt, or the
// load failed — as opposed to Caddy not running. internal/caddy rolls a
// service's routing files back only on this error.
var ErrInvalidConfig = errors.New("caddy rejected the config")

// ErrAgentDown is a reload that could not reach the agent's Caddy at all. The
// files on disk are not at fault and apply when the agent starts.
var ErrAgentDown = errors.New("the homelab agent's caddy is not reachable")

// AdminEnv overrides the admin socket path.
const AdminEnv = "HOMELAB_CADDY_ADMIN"

// AdminSocket is the unix socket of the agent's Caddy admin API: the agent's
// own Caddy is in-process, so other processes (the CLI) reload it through it.
func AdminSocket(root string) string {
	if p := os.Getenv(AdminEnv); p != "" {
		return p
	}
	return filepath.Join(root, "caddy-admin.sock")
}

// DataDir is where Caddy keeps certificates and ACME state: under the state
// root, so a restarted agent does not re-issue every certificate.
func DataDir(root string) string { return filepath.Join(root, "caddy-data") }

// Assemble returns the agent's whole Caddyfile: the embedded global options
// (admin on the unix socket, storage under root) with every caddy/l4/*.conf
// inlined in the global `layer4 { }` block and every caddy/sites/*.conf
// appended, each sorted by filename. Nothing is imported from disk at load
// time, and the legacy conf.d* directories are not read.
func Assemble(root string) (string, error) {
	base, err := assets.CoreFS.ReadFile("caddy/Caddyfile")
	if err != nil {
		return "", err
	}
	l4, err := readConfs(filepath.Join(root, "caddy", "l4"))
	if err != nil {
		return "", err
	}
	sites, err := readConfs(filepath.Join(root, "caddy", "sites"))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, line := range strings.SplitAfter(string(base), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "admin "):
			fmt.Fprintf(&b, "\tadmin %q\n", "unix/"+AdminSocket(root)+"|0600")
			fmt.Fprintf(&b, "\tstorage file_system %q\n", DataDir(root))
			// Caddy would otherwise autosave the loaded JSON — secrets
			// expanded — to $XDG_CONFIG_HOME/caddy/autosave.json.
			b.WriteString("\tpersist_config off\n")
		case trimmed == "import /homelab/caddy/l4/*.conf":
			for _, c := range l4 {
				b.WriteString(indent(c, "\t\t"))
			}
		case trimmed == "import /homelab/caddy/sites/*.conf":
			for _, c := range sites {
				b.WriteString(c)
				if !strings.HasSuffix(c, "\n") {
					b.WriteString("\n")
				}
			}
		case strings.HasPrefix(trimmed, "import /homelab/"):
			// legacy conf.d* globs: migrated away, never loaded
		default:
			b.WriteString(line)
		}
	}
	return b.String(), nil
}

// readConfs returns the contents of dir/*.conf sorted by filename; a missing
// dir is empty.
func readConfs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".conf") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // path built from the state root
		if err != nil {
			return nil, err
		}
		out = append(out, string(data))
	}
	return out, nil
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		if strings.TrimSpace(line) != "" {
			b.WriteString(prefix)
		}
		b.WriteString(line)
	}
	if !strings.HasSuffix(s, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// envPlaceholder is Caddyfile's parse-time env substitution: {$VAR} and
// {$VAR:default}.
var envPlaceholder = regexp.MustCompile(`\{\$([A-Za-z0-9_]+)(?::([^}]*))?\}`)

// expandableVars are the only placeholders expanded: what the embedded
// Caddyfile and the generated site files are built on. Any other {$VAR} — a
// routes file naming a root var or another secret — is refused rather than
// pulled into the config.
var expandableVars = map[string]bool{
	"DOMAIN": true, "HOME_SUBDOMAIN": true, "ACME_EMAIL": true, "CLOUDFLARE_API_TOKEN": true,
}

// unsafeValueChars would let a value change the Caddyfile's structure: end a
// token, open or close a block or placeholder, start a comment, open a quoted
// or backquoted string.
const unsafeValueChars = "\n\r{}#\"` \t\v\f"

// expandEnv replaces every {$VAR} from env before Caddy sees the text, so the
// adapter never consults the process environment and secrets are never
// exported into it. An unset VAR becomes its default, or "". A VAR outside
// expandableVars, or a value that could break out of its token, is an error;
// the error names the variable, never its value.
func expandEnv(text string, env map[string]string) (string, error) {
	var errs []error
	seen := map[string]bool{}
	fail := func(name string, err error) {
		if !seen[name] {
			seen[name] = true
			errs = append(errs, err)
		}
	}
	out := envPlaceholder.ReplaceAllStringFunc(text, func(m string) string {
		sub := envPlaceholder.FindStringSubmatch(m)
		name := sub[1]
		if !expandableVars[name] {
			fail(name, fmt.Errorf("placeholder {$%s} is not allowed (only DOMAIN, HOME_SUBDOMAIN, ACME_EMAIL, CLOUDFLARE_API_TOKEN)", name))
			return m
		}
		v, ok := env[name]
		if !ok {
			v = sub[2]
		}
		if strings.ContainsAny(v, unsafeValueChars) {
			fail(name, fmt.Errorf("the value of %s contains whitespace, a newline, '{', '}', '#', '\"' or '`'", name))
			return m
		}
		return v
	})
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}
	return out, nil
}

// Adapt assembles root's Caddyfile, expands its env placeholders from env in
// memory, and adapts it to Caddy's JSON. A config that does not adapt is
// ErrInvalidConfig.
func Adapt(root string, env map[string]string) ([]byte, error) {
	text, err := Assemble(root)
	if err != nil {
		return nil, err
	}
	return adaptText(text, env)
}

func adaptText(text string, env map[string]string) ([]byte, error) {
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		return nil, errors.New("caddyfile adapter not registered")
	}
	expanded, err := expandEnv(text, env)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	cfg, _, err := adapter.Adapt([]byte(expanded), map[string]any{"filename": "Caddyfile"})
	if err != nil {
		// The adapter quotes the tokens it chokes on, which after expansion
		// may be a secret's value.
		return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, redact(err.Error(), env))
	}
	return cfg, nil
}

// redact replaces the values of env's secret-named variables in msg.
func redact(msg string, env map[string]string) string {
	for k, v := range env {
		if len(v) >= 4 && secrets.IsSecret(k) {
			msg = strings.ReplaceAll(msg, v, "<"+k+">")
		}
	}
	return msg
}

// DefaultEnv is the env the Caddyfile's placeholders are filled from: root
// config.yaml vars plus their secrets (ruling R5).
func DefaultEnv(root string) (map[string]string, error) {
	sm, err := secrets.Open()
	if err != nil {
		return nil, fmt.Errorf("opening secrets: %w", err)
	}
	return config.BuildEnv(filepath.Join(root, "config.yaml"), root, "", sm)
}

// Reachable reports whether a Caddy admin API answers on sock: nil, or an
// ErrAgentDown. Check it before adapting, so a config that cannot be judged
// (no agent, so no env worth reading) is not mistaken for an invalid one.
func Reachable(sock string) error {
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return fmt.Errorf("%w (%s): %w", ErrAgentDown, sock, err)
	}
	return c.Close()
}

// PostLoad replaces the running config of the Caddy whose admin API listens
// on sock — the agent's — with cfg, forcing the reload even when unchanged.
// Caddy keeps its previous config when it rejects cfg; that is
// ErrInvalidConfig. A socket nobody answers on is ErrAgentDown.
func PostLoad(sock string, cfg []byte) error {
	client := &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}
	// Caddy accepts only loopback Host values on a unix admin socket.
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/load", bytes.NewReader(cfg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "must-revalidate")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w (%s): %w", ErrAgentDown, sock, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 400 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := strings.TrimSpace(string(body))
	var apiErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
		msg = apiErr.Error
	}
	return fmt.Errorf("%w: %s (HTTP %d)", ErrInvalidConfig, msg, resp.StatusCode)
}
