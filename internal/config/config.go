// Package config handles config-dir resolution, YAML config loading, and
// building the environment map passed to docker compose.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// DBType identifies a supported database engine.
type DBType string

const (
	DBPostgres DBType = "postgres"
	DBMariaDB  DBType = "mariadb"
	DBRedis    DBType = "redis"
)

// DBHostConfig defines how to reach a shared database instance.
type DBHostConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// DatabaseConfig maps DB type → connection endpoint for the root config.
type DatabaseConfig struct {
	Postgres *DBHostConfig `yaml:"postgres,omitempty"`
	MariaDB  *DBHostConfig `yaml:"mariadb,omitempty"`
	Redis    *DBHostConfig `yaml:"redis,omitempty"`
}

// ServiceDBDecl describes a single service's database dependency.
type ServiceDBDecl struct {
	Database   string            `yaml:"database"`
	User       string            `yaml:"user"`
	Extensions []string          `yaml:"extensions,omitempty"`
	DB         int               `yaml:"db,omitempty"` // Redis DB index
	Env        map[string]string `yaml:"env"`
}

// ServiceDatabases maps DB type → per-service DB declaration.
type ServiceDatabases map[DBType]ServiceDBDecl

// Config is the schema for config.yaml, used both at the root level and
// per-service. Vars hold non-secret plain-text values; Secrets declare which
// keyring entries are expected without storing the values.
// Groups (root config only) define named sets of services for batch operations.
// Databases: in root config, holds connection endpoints;
// in service config, holds per-service DB declarations.
// Extensions (root config only): list of enabled optional network extensions.
type Config struct {
	Vars       map[string]VarEntry    `yaml:"vars,omitempty"`
	Secrets    map[string]SecretEntry `yaml:"secrets,omitempty"`
	Groups     map[string][]string    `yaml:"groups,omitempty"`
	Databases  yaml.Node              `yaml:"databases,omitempty"`
	Extensions []string               `yaml:"extensions,omitempty"`
}

// AllExtensions returns all valid extension identifiers.
func AllExtensions() []string {
	return []string{"cf", "tor", "i2p", "yggdrasil", "ipfs"}
}

// ExtensionProfile returns the Docker Compose profile name for an extension.
func ExtensionProfile(ext string) string {
	switch ext {
	case "cf":
		return "tunnel"
	default:
		return ext
	}
}

// ExtensionLabel returns a human-readable label for an extension.
func ExtensionLabel(ext string) string {
	switch ext {
	case "cf":
		return "Cloudflare Tunnel"
	case "tor":
		return "Tor onion service proxy"
	case "i2p":
		return "I2P router + eepsite proxy"
	case "yggdrasil":
		return "Yggdrasil mesh node"
	case "ipfs":
		return "IPFS Kubo node"
	default:
		return ext
	}
}

// HasExtension reports whether an extension is enabled.
func (cfg *Config) HasExtension(name string) bool {
	for _, e := range cfg.Extensions {
		if e == name {
			return true
		}
	}
	return false
}

// EnableExtension adds an extension to the enabled list if not already present.
func (cfg *Config) EnableExtension(name string) {
	if !cfg.HasExtension(name) {
		cfg.Extensions = append(cfg.Extensions, name)
	}
}

// DisableExtension removes an extension from the enabled list.
func (cfg *Config) DisableExtension(name string) {
	filtered := make([]string, 0, len(cfg.Extensions))
	for _, e := range cfg.Extensions {
		if e != name {
			filtered = append(filtered, e)
		}
	}
	cfg.Extensions = filtered
}

// RootDatabases decodes the databases section as root-level connection endpoints.
// Returns nil when the databases section is absent.
func (cfg *Config) RootDatabases() (*DatabaseConfig, error) {
	if cfg.Databases.Kind == 0 {
		return nil, nil
	}
	var dc DatabaseConfig
	if err := cfg.Databases.Decode(&dc); err != nil {
		return nil, fmt.Errorf("decoding database config: %w", err)
	}
	return &dc, nil
}

// ServiceDatabases decodes the databases section as per-service DB declarations.
// Returns nil when the databases section is absent.
func (cfg *Config) ServiceDatabases() (ServiceDatabases, error) {
	if cfg.Databases.Kind == 0 {
		return nil, nil
	}
	var sd ServiceDatabases
	if err := cfg.Databases.Decode(&sd); err != nil {
		return nil, fmt.Errorf("decoding service database declarations: %w", err)
	}
	return sd, nil
}

// DBHost resolves the host field from either a DBHostConfig or by returning
// the shared container hostname for the given DB type.
func (dc *DatabaseConfig) DBHost(t DBType) string {
	switch t {
	case DBPostgres:
		if dc.Postgres != nil {
			return dc.Postgres.Host
		}
	case DBMariaDB:
		if dc.MariaDB != nil {
			return dc.MariaDB.Host
		}
	case DBRedis:
		if dc.Redis != nil {
			return dc.Redis.Host
		}
	}
	return ""
}

// DBPort resolves the port field from a DBHostConfig.
func (dc *DatabaseConfig) DBPort(t DBType) int {
	switch t {
	case DBPostgres:
		if dc.Postgres != nil {
			return dc.Postgres.Port
		}
		return 5432
	case DBMariaDB:
		if dc.MariaDB != nil {
			return dc.MariaDB.Port
		}
		return 3306
	case DBRedis:
		if dc.Redis != nil {
			return dc.Redis.Port
		}
		return 6379
	}
	return 0
}

// DBPasswordKey returns the root keyring key for a service's DB password.
func DBPasswordKey(svcName string) string {
	return "DB_PASSWORD_" + strings.ToUpper(strings.ReplaceAll(svcName, "-", "_"))
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
//  5. Database connection vars  (injected when service declares DB deps)
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

	// 5. Database connection vars (when service declares DB deps)
	if rootCfg != nil && svcCfg != nil {
		rootDB, err := rootCfg.RootDatabases()
		if err == nil && rootDB != nil {
			svcDB, err := svcCfg.ServiceDatabases()
			if err == nil && svcDB != nil {
				injectDBEnv(env, rootDB, svcDB, svcName, sm)
			}
		}
	}

	return env, nil
}

// injectDBEnv appends database connection variables into env.
func injectDBEnv(env map[string]string, rootDB *DatabaseConfig, svcDB ServiceDatabases, svcName string, sm *secrets.Manager) {
	for dbType, decl := range svcDB {
		host := rootDB.DBHost(dbType)
		if host == "" {
			continue
		}
		port := rootDB.DBPort(dbType)

		password := ""
		if sm != nil {
			password, _ = sm.Get("", DBPasswordKey(svcName))
		}

		portStr := fmt.Sprintf("%d", port)

		for logical, target := range decl.Env {
			if target == "" {
				continue
			}
			switch logical {
			case "host":
				env[target] = host
			case "port":
				env[target] = portStr
			case "user":
				env[target] = decl.User
			case "password":
				env[target] = password
			case "database":
				env[target] = decl.Database
			default:
				// DSN template: target is the env var name, logical is the template
				if strings.Contains(logical, "://") || strings.Contains(logical, "{user}") {
					dsn := logical
					dsn = strings.ReplaceAll(dsn, "{host}", host)
					dsn = strings.ReplaceAll(dsn, "{port}", portStr)
					dsn = strings.ReplaceAll(dsn, "{user}", decl.User)
					dsn = strings.ReplaceAll(dsn, "{password}", password)
					dsn = strings.ReplaceAll(dsn, "{database}", decl.Database)
					env[target] = dsn
				}
			}
		}
	}
}
