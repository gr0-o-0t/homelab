package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/secrets"
)

// The non-interactive face of `homelab setup`, used by the TUI and GUI and by
// scripts: --json reads what is declared, --set and --secrets-stdin write it.
// Secret values only ever travel over stdin, never argv, and are never printed.

var setupFlags struct {
	set          []string
	secretsStdin bool
}

// openSecrets opens the keyring; a variable so tests can use an in-memory one.
var openSecrets = secrets.Open

func setupNonInteractive() bool {
	return len(setupFlags.set) > 0 || setupFlags.secretsStdin
}

func checkSetupFlags() error {
	if rootFlags.json && setupNonInteractive() {
		return fmt.Errorf("--json reads the settings; it cannot be combined with --set or --secrets-stdin")
	}
	return nil
}

// SetupVar and SetupSecret are the `homelab setup --json` schema.
type SetupVar struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// SetupSecret reports whether a secret is stored, never its value.
type SetupSecret struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	Set         bool   `json:"set"`
	// Generated secrets are minted by homelab; setup does not ask for them.
	Generated bool `json:"generated,omitempty"`
}

// SetupInfo is the document `homelab setup [service] --json` prints.
type SetupInfo struct {
	Service string        `json:"service,omitempty"` // "" for the root config
	Vars    []SetupVar    `json:"vars"`
	Secrets []SetupSecret `json:"secrets"`
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// withRootDeclarations adds every root setting setup knows about that cfg
// does not declare yet, with its default, so it can be listed and set.
func withRootDeclarations(cfg *config.Config) *config.Config {
	def := rootSetupDefaults()
	if cfg.Vars == nil {
		cfg.Vars = map[string]config.VarEntry{}
	}
	if cfg.Secrets == nil {
		cfg.Secrets = map[string]config.SecretEntry{}
	}
	for k, v := range def.Vars {
		if _, ok := cfg.Vars[k]; !ok {
			cfg.Vars[k] = v
		}
	}
	for k, v := range def.Secrets {
		if _, ok := cfg.Secrets[k]; !ok {
			cfg.Secrets[k] = v
		}
	}
	return cfg
}

// printSetupJSON prints what svc ("" for root) declares; secrets as set/unset.
func printSetupJSON(w io.Writer, svc string, cfg *config.Config, sm *secrets.Manager) error {
	describe := func(name, d string) string {
		if d == "" && svc == "" {
			return rootSetupLabels[name]
		}
		return d
	}
	info := SetupInfo{Service: svc, Vars: []SetupVar{}, Secrets: []SetupSecret{}}
	for _, k := range sortedKeys(cfg.Vars) {
		e := cfg.Vars[k]
		info.Vars = append(info.Vars, SetupVar{Name: k, Value: e.Value, Required: e.Required, Description: describe(k, e.Description)})
	}
	for _, k := range sortedKeys(cfg.Secrets) {
		e := cfg.Secrets[k]
		info.Secrets = append(info.Secrets, SetupSecret{
			Name: k, Required: e.Required, Description: describe(k, e.Description),
			Set: sm.IsSet(svc, k), Generated: e.Generate != "",
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(info)
}

// applySetup writes --set vars into cfg and --secrets-stdin secrets into the
// keyring under namespace svc. Everything is validated before anything is
// written, so a typo in one name changes nothing. cfg is saved by the caller.
func applySetup(cmd *cobra.Command, cfg *config.Config, svc string, sm *secrets.Manager) error {
	vars := map[string]string{}
	for _, kv := range setupFlags.set {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return fmt.Errorf("--set %q: want KEY=VALUE", kv)
		}
		if _, declared := cfg.Vars[k]; !declared {
			if _, secret := cfg.Secrets[k]; secret {
				return fmt.Errorf("--set %s: %s is a secret — pass it with --secrets-stdin", k, k)
			}
			return fmt.Errorf("--set %s: not a declared variable (declared: %s)", k, strings.Join(sortedKeys(cfg.Vars), ", "))
		}
		vars[k] = v
	}

	var secretVals map[string]string
	if setupFlags.secretsStdin {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return fmt.Errorf("reading secrets from stdin: %w", err)
		}
		if err := json.Unmarshal(data, &secretVals); err != nil {
			return fmt.Errorf(`--secrets-stdin: want a JSON object {"NAME":"value"}: %w`, err)
		}
		for k := range secretVals {
			if _, declared := cfg.Secrets[k]; !declared {
				return fmt.Errorf("--secrets-stdin: %s is not a declared secret (declared: %s)",
					k, strings.Join(sortedKeys(cfg.Secrets), ", "))
			}
		}
	}

	for k, v := range vars {
		e := cfg.Vars[k]
		e.Value = v
		cfg.Vars[k] = e
	}
	for _, k := range sortedKeys(secretVals) {
		v := strings.TrimSpace(secretVals[k])
		if v == "" {
			continue // like pressing Enter at the prompt: keep what is stored
		}
		if err := sm.Set(svc, k, v); err != nil {
			return fmt.Errorf("storing %s in keyring: %w", k, err)
		}
	}
	return nil
}
