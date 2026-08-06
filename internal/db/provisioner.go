// Package db manages shared database provisioning for homelab services.
//
// When a service declares a database dependency (postgres, mariadb, or redis)
// in its config.yaml, the provisioner creates the database and user on the
// shared instance via docker exec. Connection strings are injected at runtime
// by config.BuildEnv().
package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/run"
)

// executor abstracts command execution so tests can inject fakes.
type executor interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
}

// secretsManager abstracts the keyring so tests can inject fakes.
type secretsManager interface {
	Get(namespace, key string) (string, error)
	Set(namespace, key, value string) error
}

// Provisioner manages database lifecycle on shared instances.
type Provisioner struct {
	ConfigDir string
	SM        secretsManager
	RC        executor
	// PollInterval overrides WaitHealthy's re-inspect delay. Zero means the
	// default; tests set it small so they need not sleep.
	PollInterval time.Duration
}

// New creates a Provisioner.
func New(cfgDir string, sm secretsManager) *Provisioner {
	return &Provisioner{
		ConfigDir: cfgDir,
		SM:        sm,
		RC:        run.Default(),
	}
}

// Provision creates a database and user for a service on the shared DB instance.
// It generates a random password, stores it in the root keyring, and runs SQL
// via docker exec. Idempotent — safe to call multiple times.
func (p *Provisioner) Provision(ctx context.Context, dbType config.DBType, svcName string, decl config.ServiceDBDecl) error {
	password, err := p.ensurePassword(svcName)
	if err != nil {
		return fmt.Errorf("generating password for %s: %w", svcName, err)
	}

	switch dbType {
	case config.DBPostgres:
		return p.provisionPostgres(ctx, svcName, decl, password)
	case config.DBMariaDB:
		return p.provisionMariaDB(ctx, svcName, decl, password)
	case config.DBRedis:
		// Redis needs no provisioning per se — just key namespace convention.
		return nil
	default:
		return fmt.Errorf("unsupported database type: %s", dbType)
	}
}

// Deprovision removes a database user from the shared instance.
// Leaves the database data intact for safety.
func (p *Provisioner) Deprovision(ctx context.Context, dbType config.DBType, svcName string, decl config.ServiceDBDecl) error {
	switch dbType {
	case config.DBPostgres:
		return p.deprovisionPostgres(ctx, decl)
	case config.DBMariaDB:
		return p.deprovisionMariaDB(ctx, decl)
	case config.DBRedis:
		return nil
	default:
		return fmt.Errorf("unsupported database type: %s", dbType)
	}
}

// EnsureRunning checks that a shared DB container is healthy.
// Returns a user-friendly error if the container isn't running.
func (p *Provisioner) EnsureRunning(ctx context.Context, dbType config.DBType) error {
	container := p.containerName(dbType)
	if container == "" {
		return fmt.Errorf("unknown database type: %s", dbType)
	}

	out, err := p.RC.Output("docker", "inspect", "--format={{.State.Status}}", container)
	if err != nil {
		return fmt.Errorf("%s container %q not found or not running\n  Install: homelab add %s && homelab up %s",
			dbType, container, dbType, dbType)
	}
	status := strings.TrimSpace(string(out))
	if status != "running" {
		return fmt.Errorf("%s container %q is %s, not running\n  Start: homelab up %s",
			dbType, container, status, dbType)
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// healthPollInterval is how often WaitHealthy re-inspects the container.
// Overridable so tests need not sleep.
const healthPollInterval = time.Second

// WaitHealthy blocks until the shared DB container for dbType reports healthy,
// or until timeout elapses.
//
// `docker compose up -d` returns as soon as the container is created, long
// before Postgres has finished recovery and is accepting connections. Starting a
// dependent service in that window gives it connection-refused errors that look
// like misconfiguration, so callers that auto-start a dependency must wait here
// first.
//
// A container with no healthcheck reports no health state at all; for those,
// "running" is the best signal available and is accepted.
func (p *Provisioner) WaitHealthy(ctx context.Context, dbType config.DBType, timeout time.Duration) error {
	container := p.containerName(dbType)
	if container == "" {
		return fmt.Errorf("unknown database type: %s", dbType)
	}

	interval := p.PollInterval
	if interval <= 0 {
		interval = healthPollInterval
	}

	deadline := time.Now().Add(timeout)
	var last string
	for {
		out, err := p.RC.Output("docker", "inspect",
			"--format={{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", container)
		if err == nil {
			switch last = strings.TrimSpace(string(out)); last {
			case "healthy", "running":
				return nil
			case "unhealthy":
				// Keep waiting: an unhealthy report during start_period is normal.
			}
		}

		if !time.Now().Before(deadline) {
			if last == "" {
				last = "no status reported"
			}
			return fmt.Errorf("%s container %q did not become healthy within %s (last status: %s)\n"+
				"  Check: homelab logs %s\n"+
				"  If it never starts, confirm `homelab setup %s` has been run — "+
				"the image needs its root password to initialise",
				dbType, container, timeout, last,
				config.SharedDBName(dbType), config.SharedDBName(dbType))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (p *Provisioner) containerName(dbType config.DBType) string {
	switch dbType {
	case config.DBPostgres:
		return "homelab-postgres"
	case config.DBMariaDB:
		return "homelab-mariadb"
	case config.DBRedis:
		return "homelab-redis"
	default:
		return ""
	}
}

func (p *Provisioner) ensurePassword(svcName string) (string, error) {
	key := config.DBPasswordKey(svcName)
	if p.SM != nil {
		existing, _ := p.SM.Get("", key)
		if existing != "" {
			return existing, nil
		}
	}
	pass := generatePassword(32)
	if p.SM != nil {
		if err := p.SM.Set("", key, pass); err != nil {
			return "", fmt.Errorf("storing password in keyring: %w", err)
		}
	}
	return pass, nil
}

func (p *Provisioner) execSQL(container, user, sql string) error {
	var args []string
	if strings.Contains(container, "postgres") {
		args = []string{"exec", "-i", container, "psql", "-U", user, "-c", sql}
	} else {
		args = []string{"exec", "-i", container, "mysql", "-u", "root", "-e", sql}
	}
	return p.RC.Run("docker", args...)
}

// execPSQL runs a psql command with database context and returns output.
func (p *Provisioner) execPSQL(container, db, user string, args ...string) ([]byte, error) {
	psqlArgs := []string{"exec", container, "psql", "-U", user, "-d", db, "-t", "-A"}
	psqlArgs = append(psqlArgs, args...)
	return p.RC.Output("docker", psqlArgs...)
}

func (p *Provisioner) provisionPostgres(ctx context.Context, svcName string, decl config.ServiceDBDecl, password string) error {
	container := p.containerName(config.DBPostgres)

	// Ownership model: one database per service, owned outright by that
	// service's role. Every database here is single-tenant, so an owner role is
	// both simpler and closer to what upstream images expect than the
	// grant-only setup this replaced: the service can create its own schemas,
	// types and extensions without further privileges, and pg_dump/pg_restore
	// round-trip without --no-owner. The role is therefore created before the
	// database, so CREATE DATABASE can name it as OWNER.

	// 1. Create the role, or reset its password if it is already there.
	// PostgreSQL has no CREATE USER IF NOT EXISTS — unlike MySQL, that spelling
	// is a plain syntax error (see the CREATE ROLE grammar), so the role has to
	// be looked up first. ALTER on the existing-role path also re-syncs the
	// password with the keyring, making a repeated `homelab setup` idempotent.
	out, _ := p.execPSQL(container, "postgres", "postgres",
		"-c", fmt.Sprintf("SELECT 1 FROM pg_roles WHERE rolname=%s", escLit(decl.User)))
	verb := "CREATE"
	if strings.TrimSpace(string(out)) != "" {
		verb = "ALTER"
	}
	if err := p.RC.Run("docker", "exec", container, "psql", "-U", "postgres",
		"-c", fmt.Sprintf("%s USER %s WITH LOGIN PASSWORD %s",
			verb, escID(decl.User), escLit(password))); err != nil {
		return fmt.Errorf("creating user %s: %w", decl.User, err)
	}

	// 2. Optionally promote to superuser. Ownership covers schema and extension
	// creation; this is only for services that go further — upgrading an
	// extension in place, or backing up with pg_dumpall.
	if decl.Superuser {
		if err := p.RC.Run("docker", "exec", container, "psql", "-U", "postgres",
			"-c", fmt.Sprintf("ALTER USER %s WITH SUPERUSER", escID(decl.User))); err != nil {
			return fmt.Errorf("granting superuser to %s: %w", decl.User, err)
		}
	}

	// 3. Create the database owned by that role, or transfer an existing one.
	out, _ = p.execPSQL(container, "postgres", "postgres",
		"-c", fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname=%s", escLit(decl.Database)))
	if strings.TrimSpace(string(out)) == "" {
		if err := p.RC.Run("docker", "exec", container, "psql", "-U", "postgres",
			"-c", fmt.Sprintf("CREATE DATABASE %s OWNER %s",
				escID(decl.Database), escID(decl.User))); err != nil {
			return fmt.Errorf("creating database %s: %w", decl.Database, err)
		}
	} else if err := p.RC.Run("docker", "exec", container, "psql", "-U", "postgres",
		"-c", fmt.Sprintf("ALTER DATABASE %s OWNER TO %s",
			escID(decl.Database), escID(decl.User))); err != nil {
		return fmt.Errorf("setting owner of database %s: %w", decl.Database, err)
	}

	// 4. Hand over the public schema too. Since PG 15 it is owned by
	// pg_database_owner and no longer world-writable, so a database transferred
	// by the ALTER above still needs this to let the service create tables.
	if err := p.RC.Run("docker", "exec", container, "psql", "-U", "postgres", "-d", decl.Database,
		"-c", fmt.Sprintf("ALTER SCHEMA public OWNER TO %s", escID(decl.User))); err != nil {
		return fmt.Errorf("setting owner of schema public in %s: %w", decl.Database, err)
	}

	// 5. Extensions. Created as the superuser so services whose own role is
	// unprivileged still find them present; ordering is preserved so a
	// dependency can be listed before the extension that needs it
	// (e.g. cube before earthdistance).
	for _, ext := range decl.Extensions {
		if err := p.RC.Run("docker", "exec", container, "psql", "-U", "postgres", "-d", decl.Database,
			"-c", fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %q", ext)); err != nil {
			return fmt.Errorf("creating extension %s: %w", ext, err)
		}
	}

	return nil
}

func (p *Provisioner) deprovisionPostgres(ctx context.Context, decl config.ServiceDBDecl) error {
	container := p.containerName(config.DBPostgres)
	sql := fmt.Sprintf(`DROP USER IF EXISTS %s;`, escID(decl.User))
	return p.execSQL(container, "postgres", sql)
}

// mariaAnyHost is MariaDB's "connect from anywhere" host pattern. Provision and
// deprovision must spell it identically: the host is stored verbatim as part of
// the account identity, so creating user@'%%' and later dropping user@'%' leaves
// the account (and its grants) behind. Keeping it in one constant is what stops
// the two from drifting again — the previous code wrote '%%%%' in one Sprintf
// format and '%%' in the other, which render as '%%' and '%'.
const mariaAnyHost = "%"

func (p *Provisioner) provisionMariaDB(ctx context.Context, svcName string, decl config.ServiceDBDecl, password string) error {
	container := p.containerName(config.DBMariaDB)
	sql := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`; "+
		"CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED BY '%s'; "+
		"GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%s'; "+
		"FLUSH PRIVILEGES;",
		decl.Database, decl.User, mariaAnyHost, password,
		decl.Database, decl.User, mariaAnyHost)
	return p.execSQL(container, "root", sql)
}

func (p *Provisioner) deprovisionMariaDB(ctx context.Context, decl config.ServiceDBDecl) error {
	container := p.containerName(config.DBMariaDB)
	sql := fmt.Sprintf(`DROP USER IF EXISTS '%s'@'%s';`, decl.User, mariaAnyHost)
	return p.execSQL(container, "root", sql)
}

// escLit renders a value as a single-quoted SQL string literal, doubling any
// embedded quote. Use it for values — WHERE comparisons and passwords — where
// escID's double quotes would be wrong. Generated passwords are alphanumeric,
// but database and role names come from a service's config.yaml, so neither
// should be pasted in raw.
func escLit(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// escID wraps an identifier in double quotes, doubling any internal quotes.
func escID(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
