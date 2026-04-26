// Package config handles config-dir resolution, YAML config loading, and
// building the environment map passed to docker compose.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/secrets"
	"gopkg.in/yaml.v3"
)

// VarEntry is one non-secret variable declaration.
// The value is stored in plain text in config.yaml.
type VarEntry struct {
	Value    string `yaml:"value"`
	Required bool   `yaml:"required"`
}

// SecretEntry declares a secret variable. Its value lives only in the system
// keyring — never written to config.yaml or any file on disk.
type SecretEntry struct {
	Required bool `yaml:"required"`
}

// Config is the schema for config.yaml, used both at the root level and
// per-service. Vars hold non-secret plain-text values; Secrets declare which
// keyring entries are expected without storing the values.
// Groups (root config only) define named sets of services for batch operations.
type Config struct {
	Vars    map[string]VarEntry    `yaml:"vars,omitempty"`
	Secrets map[string]SecretEntry `yaml:"secrets,omitempty"`
	Groups  map[string][]string    `yaml:"groups,omitempty"`
}

// configFileName is the canonical name for all config files.
const configFileName = "config.yaml"

// Load reads a config.yaml at path. Returns (nil, nil) if the file does not
// exist — callers treat absence as an empty config, not an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes cfg to path, creating parent directories as needed.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// RootConfigFile returns the effective root config file path.
//
//	configFile overrides configDir/config.yaml when non-empty (--config flag).
func RootConfigFile(configDir, configFile string) string {
	if configFile != "" {
		return configFile
	}
	return filepath.Join(configDir, configFileName)
}

// ServiceConfigFile returns the config.yaml path for a named service.
func ServiceConfigFile(configDir, svcName string) string {
	return filepath.Join(configDir, "services", svcName, configFileName)
}

// BuildEnv returns the combined environment map for docker compose.
//
// Loading order (later wins — service overrides root):
//  1. Root config.yaml vars
//  2. Root keyring secrets
//  3. Service config.yaml vars  (overrides root vars)
//  4. Service keyring secrets   (overrides root secrets)
//
// Pass sm=nil to skip all keyring lookups.
func BuildEnv(rootConfigFile, configDir, svcName string, sm *secrets.Manager) (map[string]string, error) {
	env := make(map[string]string)

	// 1. Root vars from config.yaml
	rootCfg, err := Load(rootConfigFile)
	if err != nil {
		return nil, err
	}
	if rootCfg != nil {
		for k, v := range rootCfg.Vars {
			if v.Value != "" {
				env[k] = v.Value
			}
		}
	}

	// 2. Root secrets from keyring
	if sm != nil && rootCfg != nil {
		for k := range rootCfg.Secrets {
			if val, _ := sm.Get("", k); val != "" {
				env[k] = val
			}
		}
	}

	if svcName == "" {
		return env, nil
	}

	// 3. Service vars (override root)
	svcCfg, err := Load(ServiceConfigFile(configDir, svcName))
	if err != nil {
		return nil, err
	}
	if svcCfg != nil {
		for k, v := range svcCfg.Vars {
			if v.Value != "" {
				env[k] = v.Value
			}
		}

		// 4. Service secrets from keyring (override root secrets)
		if sm != nil {
			for k := range svcCfg.Secrets {
				if val, _ := sm.Get(svcName, k); val != "" {
					env[k] = val
				}
			}
		}
	}

	return env, nil
}
