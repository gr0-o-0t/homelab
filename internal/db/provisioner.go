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
		return fmt.Errorf("%s container %q not found or not running\n  Install: homelab service add %s && homelab service up %s",
			dbType, container, dbType, dbType)
	}
	status := strings.TrimSpace(string(out))
	if status != "running" {
		return fmt.Errorf("%s container %q is %s, not running\n  Start: homelab service up %s",
			dbType, container, status, dbType)
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

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

	// 1. Check if database exists, create if not.
	out, _ := p.execPSQL(container, "postgres", "postgres",
		"-c", fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname='%s'", decl.Database))
	if strings.TrimSpace(string(out)) == "" {
		_ = p.RC.Run("docker", "exec", container, "psql", "-U", "postgres",
			"-c", fmt.Sprintf("CREATE DATABASE %s", escID(decl.Database)))
	}

	// 2. Create user if not exists (PG 15+).
	_ = p.RC.Run("docker", "exec", container, "psql", "-U", "postgres",
		"-c", fmt.Sprintf("CREATE USER IF NOT EXISTS %s WITH PASSWORD '%s'", escID(decl.User), password))

	// 3. Grant privileges.
	_ = p.RC.Run("docker", "exec", container, "psql", "-U", "postgres",
		"-c", fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s", escID(decl.Database), escID(decl.User)))
	_ = p.RC.Run("docker", "exec", container, "psql", "-U", "postgres", "-d", decl.Database,
		"-c", fmt.Sprintf("GRANT ALL ON SCHEMA public TO %s", escID(decl.User)))

	// 4. Extensions.
	for _, ext := range decl.Extensions {
		_ = p.RC.Run("docker", "exec", container, "psql", "-U", "postgres", "-d", decl.Database,
			"-c", fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS \"%s\"", ext))
	}

	return nil
}

func (p *Provisioner) deprovisionPostgres(ctx context.Context, decl config.ServiceDBDecl) error {
	container := p.containerName(config.DBPostgres)
	sql := fmt.Sprintf(`DROP USER IF EXISTS %s;`, escID(decl.User))
	return p.execSQL(container, "postgres", sql)
}

func (p *Provisioner) provisionMariaDB(ctx context.Context, svcName string, decl config.ServiceDBDecl, password string) error {
	container := p.containerName(config.DBMariaDB)
	sql := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`; "+
		"CREATE USER IF NOT EXISTS '%s'@'%%%%' IDENTIFIED BY '%s'; "+
		"GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%%%'; "+
		"FLUSH PRIVILEGES;",
		decl.Database, decl.User, password, decl.Database, decl.User)
	return p.execSQL(container, "root", sql)
}

func (p *Provisioner) deprovisionMariaDB(ctx context.Context, decl config.ServiceDBDecl) error {
	container := p.containerName(config.DBMariaDB)
	sql := fmt.Sprintf(`DROP USER IF EXISTS '%s'@'%%';`, decl.User)
	return p.execSQL(container, "root", sql)
}

// escID wraps an identifier in double quotes, doubling any internal quotes.
func escID(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
