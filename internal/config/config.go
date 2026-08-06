// Package config handles config-dir resolution, YAML config loading, and
// building the environment map passed to docker compose.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// PortEntry describes a single port a service exposes.
//
// The declaration grammar is one line per exposed port:
//
//	8080            → <service>.<home>.<domain>            → container :8080
//	80:8080         → <service>.<home>.<domain>:80         → container :8080
//	vault:80        → vault.<home>.<domain>                → container :80
//	22:22/tcp       → as above, restricted to one protocol
//
// The token left of the colon decides which of the two middle forms applies:
// all digits means "listen on this port", anything else means "serve under this
// subdomain". A subdomain *replaces* the service name rather than prefixing it,
// which is how a service can answer on a name that isn't its directory name —
// vaultwarden serving vault.<home>.<domain> was the case that forced this.
type PortEntry struct {
	// Port is the container port traffic is forwarded to.
	Port int `yaml:"port"`

	// Listen is the port clients connect on. Zero means the default site port
	// (443 for the tailnet layer, i.e. no explicit port in the site address).
	Listen int `yaml:"listen,omitempty"`

	// Subdomain replaces the service name in the site address. Empty means the
	// service name is used.
	Subdomain string `yaml:"subdomain,omitempty"`

	// Protocols is "tcp", "udp" or both — both when the declaration says
	// nothing. Caddy only routes tcp; udp entries are recorded for compose and
	// documentation, and skipped when generating site blocks.
	Protocols []string `yaml:"protocols,omitempty"`

	// Protocol is the legacy single-protocol field, still accepted in the
	// mapping format.
	Protocol string `yaml:"protocol,omitempty"`
}

// HasTCP reports whether this port carries TCP, which is the only thing Caddy
// can route.
func (e PortEntry) HasTCP() bool {
	if len(e.Protocols) == 0 {
		return e.Protocol == "" || strings.EqualFold(e.Protocol, "tcp")
	}
	for _, p := range e.Protocols {
		if strings.EqualFold(p, "tcp") {
			return true
		}
	}
	return false
}

// PortEntries accepts ports config in both the legacy mapping format and the
// list-of-strings format described on PortEntry. Internal representation is
// always a map keyed by the token left of the colon ("default" when bare), so
// each declaration gets a stable name for --ports selection and for the
// per-port filenames generated config uses.
type PortEntries map[string]PortEntry

// UnmarshalYAML accepts both the list format and the legacy mapping format.
func (p *PortEntries) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		return p.decodeNew(value)
	case yaml.MappingNode:
		return p.decodeLegacy(value)
	default:
		return fmt.Errorf("ports: expected sequence or mapping, got kind %d", value.Kind)
	}
}

func (p *PortEntries) decodeLegacy(value *yaml.Node) error {
	var old map[string]PortEntry
	if err := value.Decode(&old); err != nil {
		return fmt.Errorf("decoding legacy ports format: %w", err)
	}
	*p = PortEntries(old)
	return nil
}

func (p *PortEntries) decodeNew(value *yaml.Node) error {
	*p = make(PortEntries)
	for _, item := range value.Content {
		var s string
		if err := item.Decode(&s); err != nil {
			return fmt.Errorf("decoding port entry: %w", err)
		}
		key, entry, err := ParsePortString(s)
		if err != nil {
			return err
		}
		if _, exists := (*p)[key]; exists {
			return fmt.Errorf("port config %q: %q is declared twice", s, key)
		}
		(*p)[key] = entry
	}
	return nil
}

// ParsePortString parses one port declaration. See PortEntry for the grammar.
func ParsePortString(s string) (key string, entry PortEntry, err error) {
	spec := strings.TrimSpace(s)
	if spec == "" {
		return "", PortEntry{}, fmt.Errorf("empty port string")
	}

	// Optional /tcp, /udp, or /tcp/udp suffix. Absent means both.
	protocols := []string{"tcp", "udp"}
	if i := strings.Index(spec, "/"); i >= 0 {
		protocols = nil
		for _, proto := range strings.Split(spec[i+1:], "/") {
			proto = strings.ToLower(strings.TrimSpace(proto))
			if proto != "tcp" && proto != "udp" {
				return "", PortEntry{}, fmt.Errorf("invalid protocol %q in %q: want tcp or udp", proto, s)
			}
			protocols = append(protocols, proto)
		}
		if len(protocols) == 0 {
			return "", PortEntry{}, fmt.Errorf("invalid port format %q: empty protocol", s)
		}
		spec = spec[:i]
	}

	parts := strings.Split(spec, ":")
	if len(parts) > 2 {
		return "", PortEntry{}, fmt.Errorf("invalid port format %q: too many colons", s)
	}

	port, err := parsePortNumber(parts[len(parts)-1], s)
	if err != nil {
		return "", PortEntry{}, err
	}
	entry = PortEntry{Port: port, Protocols: protocols}

	if len(parts) == 1 {
		return "default", entry, nil
	}

	left := strings.TrimSpace(parts[0])
	if left == "" {
		return "", PortEntry{}, fmt.Errorf("invalid port format %q: empty segment", s)
	}
	if isNumeric(left) {
		listen, err := parsePortNumber(left, s)
		if err != nil {
			return "", PortEntry{}, err
		}
		entry.Listen = listen
		return left, entry, nil
	}
	entry.Subdomain = left
	return left, entry, nil
}

func parsePortNumber(s, spec string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid port number %q in %q", s, spec)
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d out of range (1-65535) in %q", p, spec)
	}
	return p, nil
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
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
// Host/Port: when set, override root-level shared DB host/port
// (e.g. for local DB containers defined in the service's compose file).
// DSNTemplate: custom DSN template; use {host}/{port}/{user}/{password}/{database}
// placeholders. Omit to use the per-type default template.
// Superuser: grant the service's role SUPERUSER on the shared instance. Only
// for services that manage extensions themselves at runtime (Immich checks and
// upgrades its vector extension on every start, and its backup path shells out
// to pg_dumpall) — pre-creating extensions via Extensions is otherwise enough.
type ServiceDBDecl struct {
	Database    string            `yaml:"database,omitempty"`
	User        string            `yaml:"user,omitempty"`
	Host        string            `yaml:"host,omitempty"`
	Port        int               `yaml:"port,omitempty"`
	DSNTemplate string            `yaml:"dsn_template,omitempty"`
	Extensions  []string          `yaml:"extensions,omitempty"`
	Superuser   bool              `yaml:"superuser,omitempty"`
	Env         map[string]string `yaml:"env"`
}

// TypedDBDecl pairs a database type with its declaration.
// This flat structure allows multiple instances of the same DB type.
type TypedDBDecl struct {
	Type DBType
	ServiceDBDecl
}

// ServiceDatabases is a flat list of database declarations.
// Supports multiple instances of the same DB type.
// YAML accepts both a sequence (new format) and a mapping (legacy format).
type ServiceDatabases []TypedDBDecl

// DBTypeSet returns the set of unique DB types in this declaration list.
func (d ServiceDatabases) DBTypeSet() map[DBType]bool {
	set := make(map[DBType]bool, len(d))
	for i := range d {
		set[d[i].Type] = true
	}
	return set
}

// UnmarshalYAML accepts both the new sequence format and the legacy mapping format.
//
// New format (recommended):
//
//	databases:
//	  - postgres:
//	      database: forgejo
//	      env: { ... }
//	  - redis:
//	      env: { ... }
//
// Legacy format (backward compatible):
//
//	databases:
//	  postgres:
//	    database: forgejo
//	    env: { ... }
func (d *ServiceDatabases) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		return d.decodeSequence(value)
	case yaml.MappingNode:
		return d.decodeLegacy(value)
	default:
		return fmt.Errorf("databases: expected sequence or mapping, got kind %d", value.Kind)
	}
}

func (d *ServiceDatabases) decodeSequence(value *yaml.Node) error {
	for i, item := range value.Content {
		// Each item: { postgres: { database: ..., env: ... } }
		// item is a mapping node with one key-value pair
		if item.Kind != yaml.MappingNode || len(item.Content) < 2 {
			return fmt.Errorf("databases: entry %d (line %d) is not a valid %q-style mapping", i, item.Line, "type: {options}")
		}
		var dbType DBType
		if err := item.Content[0].Decode(&dbType); err != nil {
			return fmt.Errorf("decoding database type: %w", err)
		}
		var decl ServiceDBDecl
		if err := item.Content[1].Decode(&decl); err != nil {
			return fmt.Errorf("decoding declaration for %q: %w", dbType, err)
		}
		*d = append(*d, TypedDBDecl{Type: dbType, ServiceDBDecl: decl})
	}
	return nil
}

func (d *ServiceDatabases) decodeLegacy(value *yaml.Node) error {
	var old map[DBType]ServiceDBDecl
	if err := value.Decode(&old); err != nil {
		return fmt.Errorf("decoding legacy database format: %w", err)
	}
	for dbType, decl := range old {
		*d = append(*d, TypedDBDecl{Type: dbType, ServiceDBDecl: decl})
	}
	return nil
}

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
	Ports      PortEntries            `yaml:"ports,omitempty"`
}

// extensionAliases maps config.yaml extension names to registry (canonical) names.
var extensionAliases = map[string]string{
	"yggdrasil": "ygg",
	"i2pd":      "i2p",
	"ts":        "ts",
}

// ResolveExtension resolves a config.yaml extension name to the canonical
// registry name. Unknown names are returned unchanged so they can produce
// a useful "not found" error from the registry.
func ResolveExtension(name string) string {
	if resolved, ok := extensionAliases[name]; ok {
		return resolved
	}
	return name
}

// AllExtensions returns all valid extension identifiers (canonical names).
func AllExtensions() []string {
	return []string{"ts", "cf", "tor", "i2p", "ygg"}
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

// ExtensionLabel returns a human-readable label for an extension. Takes the
// canonical name (see AllExtensions/ResolveExtension) — "ygg", not the
// legacy config.yaml alias "yggdrasil".
func ExtensionLabel(ext string) string {
	switch ext {
	case "ts":
		return "Tailscale"
	case "cf":
		return "Cloudflare Tunnel"
	case "tor":
		return "Tor onion service proxy"
	case "i2p":
		return "I2P router + eepsite proxy"
	case "ygg":
		return "Yggdrasil mesh node"
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

// DBHost resolves the host for a DB type: an explicit root-level override if
// one is configured, otherwise the shared container hostname.
//
// The root `databases:` section is optional — most installs never write one, so
// falling back to SharedDBContainer here is what makes a service's `databases:`
// declaration work out of the box. Returning "" instead made injectDBEnv skip
// the entry, leaving DATABASE_URL and friends unset. Safe on a nil receiver for
// the same reason: "no root section" is the normal case, not an error.
func (dc *DatabaseConfig) DBHost(t DBType) string {
	if dc != nil {
		switch t {
		case DBPostgres:
			if dc.Postgres != nil && dc.Postgres.Host != "" {
				return dc.Postgres.Host
			}
		case DBMariaDB:
			if dc.MariaDB != nil && dc.MariaDB.Host != "" {
				return dc.MariaDB.Host
			}
		case DBRedis:
			if dc.Redis != nil && dc.Redis.Host != "" {
				return dc.Redis.Host
			}
		}
	}
	return SharedDBContainer(t)
}

// DBPort resolves the port for a DB type, falling back to the engine's default.
// Nil-receiver safe, and treats a configured 0 as "unset" so a root section that
// names only a host still yields a usable port.
func (dc *DatabaseConfig) DBPort(t DBType) int {
	if dc != nil {
		switch t {
		case DBPostgres:
			if dc.Postgres != nil && dc.Postgres.Port != 0 {
				return dc.Postgres.Port
			}
		case DBMariaDB:
			if dc.MariaDB != nil && dc.MariaDB.Port != 0 {
				return dc.MariaDB.Port
			}
		case DBRedis:
			if dc.Redis != nil && dc.Redis.Port != 0 {
				return dc.Redis.Port
			}
		}
	}
	switch t {
	case DBPostgres:
		return 5432
	case DBMariaDB:
		return 3306
	case DBRedis:
		return 6379
	}
	return 0
}

// DBPasswordKey returns the root keyring key for a service's DB password.
func DBPasswordKey(svcName string) string {
	return "DB_PASSWORD_" + strings.ToUpper(strings.ReplaceAll(svcName, "-", "_"))
}

// SharedDBName returns the service name for a shared DB type.
func SharedDBName(t DBType) string {
	switch t {
	case DBPostgres:
		return "postgres"
	case DBMariaDB:
		return "mariadb"
	case DBRedis:
		return "redis"
	default:
		return ""
	}
}

// SharedDBContainer returns the container hostname for a shared DB service.
func SharedDBContainer(t DBType) string {
	switch t {
	case DBPostgres:
		return "homelab-postgres"
	case DBMariaDB:
		return "homelab-mariadb"
	case DBRedis:
		return "homelab-redis"
	default:
		return ""
	}
}

// IsSharedDBService reports whether a service name is one of the shared
// database services (postgres, mariadb, redis).
func IsSharedDBService(name string) (DBType, bool) {
	switch name {
	case "postgres":
		return DBPostgres, true
	case "mariadb":
		return DBMariaDB, true
	case "redis":
		return DBRedis, true
	default:
		return "", false
	}
}

// EnsureRootDBConfig adds the databases section to the root config if a shared
// DB service is being added or started. Idempotent — safe to call repeatedly.
// Returns nil when svcName is not a shared DB service or config is already set.
func EnsureRootDBConfig(rootCfgFile, svcName string) error {
	dbType, ok := IsSharedDBService(svcName)
	if !ok {
		return nil // not a shared DB service
	}

	cfg, err := Load(rootCfgFile)
	if err != nil {
		return fmt.Errorf("loading root config: %w", err)
	}
	if cfg == nil {
		return nil
	}

	// Already configured?
	rootDB, err := cfg.RootDatabases()
	if err != nil {
		return err
	}
	if rootDB != nil && rootDB.DBHost(dbType) != "" {
		return nil // already set
	}

	// Build the new host config
	if rootDB == nil {
		rootDB = &DatabaseConfig{}
	}
	hc := &DBHostConfig{
		Host: SharedDBContainer(dbType),
		Port: rootDB.DBPort(dbType),
	}
	switch dbType {
	case DBPostgres:
		rootDB.Postgres = hc
	case DBMariaDB:
		rootDB.MariaDB = hc
	case DBRedis:
		rootDB.Redis = hc
	}

	// Encode DatabaseConfig back into cfg.Databases yaml.Node.
	// yaml.Unmarshal into a yaml.Node wraps the content in a DocumentNode,
	// but embedding a DocumentNode inside another YAML document fails.
	// Strip the wrapper by taking the first content child.
	data, err := yaml.Marshal(rootDB)
	if err != nil {
		return fmt.Errorf("marshaling database config: %w", err)
	}
	var docNode yaml.Node
	if err := yaml.Unmarshal(data, &docNode); err != nil {
		return fmt.Errorf("unmarshaling database config: %w", err)
	}
	if len(docNode.Content) == 0 {
		return fmt.Errorf("empty database config node")
	}
	cfg.Databases = *docNode.Content[0]

	return Save(rootCfgFile, cfg)
}

// configFileName is the canonical name for all config files.
const configFileName = "config.yaml"

// Load reads a config.yaml at path. Returns (nil, nil) if the file does not
// exist — callers treat absence as an empty config, not an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // nosec G304 -- path is CLI config path, never user-controlled
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
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
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
//
// A non-nil error means one or more secrets couldn't be read from the
// keyring due to a genuine backend failure (Manager.Get already treats
// "not set" as a nil error with an empty value) — env still contains
// everything that *did* resolve, so callers can choose to proceed with a
// partial result and surface the error as a warning rather than aborting.
func BuildEnv(rootConfigFile, configDir, svcName string, sm *secrets.Manager) (map[string]string, error) {
	env := make(map[string]string)
	var errs []error

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
			val, err := sm.Get("", k)
			if err != nil {
				errs = append(errs, fmt.Errorf("secret %q: %w", k, err))
				continue
			}
			if val != "" {
				env[k] = val
			}
		}
	}

	if svcName == "" {
		return env, errors.Join(errs...)
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
				val, err := sm.Get(svcName, k)
				if err != nil {
					errs = append(errs, fmt.Errorf("secret %q: %w", k, err))
					continue
				}
				if val != "" {
					env[k] = val
				}
			}
		}
	}

	// 5. Database connection vars (when the service declares DB deps).
	//
	// The root `databases:` section is optional and most installs have none, so
	// a nil rootDB must still inject — DBHost/DBPort resolve it to the shared
	// containers and their default ports. Gating this block on rootDB != nil
	// meant every DB-backed service silently started with an empty
	// DATABASE_URL / DB_HOSTNAME / REDIS_HOSTNAME.
	if svcCfg != nil {
		var rootDB *DatabaseConfig
		if rootCfg != nil {
			if dc, err := rootCfg.RootDatabases(); err == nil {
				rootDB = dc
			}
		}
		svcDB, err := svcCfg.ServiceDatabases()
		if err == nil && svcDB != nil {
			if err := injectDBEnv(env, rootDB, svcDB, svcName, sm); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return env, errors.Join(errs...)
}

// defaultDSNTemplate returns the default DSN template for a DB type.
func defaultDSNTemplate(t DBType) string {
	switch t {
	case DBPostgres:
		return "postgres://{user}:{password}@{host}:{port}/{database}"
	case DBMariaDB:
		return "mysql://{user}:{password}@{host}:{port}/{database}"
	case DBRedis:
		return "redis://{host}:{port}/0"
	default:
		return ""
	}
}

// buildDSN renders a DSN template by substituting
// {host}/{port}/{user}/{password}/{database} in a single pass via
// strings.NewReplacer. Sequential strings.ReplaceAll calls would re-scan
// already-substituted text, so a value that happens to contain another
// placeholder's literal token (e.g. a user name containing the substring
// "{database}") would get corrupted by a later substitution; NewReplacer
// matches all patterns against the original string in one pass instead.
func buildDSN(tmpl, host, portStr, user, password, database string) string {
	replacer := strings.NewReplacer(
		"{host}", host,
		"{port}", portStr,
		"{user}", user,
		"{password}", password,
		"{database}", database,
	)
	return replacer.Replace(tmpl)
}

// injectDBEnv appends database connection variables into env. Returns an
// error only for a genuine keyring failure reading the DB password — never
// for "not set", which Manager.Get already reports as a nil error.
func injectDBEnv(env map[string]string, rootDB *DatabaseConfig, svcDB ServiceDatabases, svcName string, sm *secrets.Manager) error {
	rootPassword := ""
	if sm != nil {
		pw, err := sm.Get("", DBPasswordKey(svcName))
		if err != nil {
			return fmt.Errorf("reading db password for %q: %w", svcName, err)
		}
		rootPassword = pw
	}

	for i := range svcDB {
		entry := &svcDB[i]
		// Resolve host: explicit host overrides root config
		host := entry.Host
		if host == "" {
			host = rootDB.DBHost(entry.Type)
		}
		if host == "" {
			continue
		}

		// Resolve port: explicit port overrides root config
		port := entry.Port
		if port == 0 {
			port = rootDB.DBPort(entry.Type)
		}
		portStr := fmt.Sprintf("%d", port)

		for logical, target := range entry.Env {
			if target == "" {
				continue
			}
			switch logical {
			case "host":
				env[target] = host
			case "port":
				env[target] = portStr
			case "user":
				env[target] = entry.User
			case "password":
				env[target] = rootPassword
			case "database":
				env[target] = entry.Database
			case "dsn":
				// Build DSN from template (custom or per-type default)
				tmpl := entry.DSNTemplate
				if tmpl == "" {
					tmpl = defaultDSNTemplate(entry.Type)
				}
				if tmpl != "" {
					env[target] = buildDSN(tmpl, host, portStr, entry.User, rootPassword, entry.Database)
				}
			default:
				// Legacy DSN template: logical is the template, target is the env var name
				if strings.Contains(logical, "://") || strings.Contains(logical, "{user}") {
					env[target] = buildDSN(logical, host, portStr, entry.User, rootPassword, entry.Database)
				}
			}
		}
	}
	return nil
}
