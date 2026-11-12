// Package db manages shared database provisioning for homelab services.
//
// When a service declares a dependency (postgres, mariadb, redis or s3) in its
// config.yaml, the provisioner isolates it on the one shared instance via
// docker exec: a database and role on postgres/mariadb, a database number on
// redis, a bucket and access key on Garage. Connection details are injected at
// runtime by config.BuildEnv().
package db

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/run"
)

// executor abstracts command execution so tests can inject fakes.
type executor interface {
	Run(name string, args ...string) error
	RunFrom(r io.Reader, name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
	OutputFrom(r io.Reader, name string, args ...string) ([]byte, error)
}

// secretsManager abstracts the keyring so tests can inject fakes.
type secretsManager interface {
	Get(namespace, key string) (string, error)
	Set(namespace, key, value string) error
	Delete(namespace, key string) error
}

// ContainerLister is the slice of the Docker API the default container
// resolver needs; *client.Client and swarm.API satisfy it.
type ContainerLister interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
}

// Starter deploys a catalog service; swarm.Deployer satisfies it.
type Starter interface {
	Up(ctx context.Context, svc string) error
}

// Provisioner manages database lifecycle on shared instances.
type Provisioner struct {
	ConfigDir string
	SM        secretsManager
	RC        executor
	// PollInterval overrides WaitHealthy's re-inspect delay. Zero means the
	// default; tests set it small so they need not sleep.
	PollInterval time.Duration
	// ContainerFor names the container to `docker exec` into for a shared
	// instance. Nil means the running Swarm task on this node, found via API.
	ContainerFor func(dbType config.DBType) (string, error)
	// API lists containers for the default ContainerFor.
	API ContainerLister
	// Deployer, when set, lets EnsureRunning start a provider that is not
	// running; nil makes EnsureRunning a check only.
	Deployer Starter
}

// New creates a Provisioner.
func New(cfgDir string, sm secretsManager) *Provisioner {
	return &Provisioner{
		ConfigDir: cfgDir,
		SM:        sm,
		RC:        run.Default(),
	}
}

// Provision isolates a service on the shared instance for dbType: a database
// and role (postgres, mariadb), a database number (redis), or a bucket and
// access key (s3). Generated credentials go to the keyring. Idempotent — safe
// to call on every start.
func (p *Provisioner) Provision(ctx context.Context, dbType config.DBType, svcName string, decl config.ServiceDBDecl) error {
	switch dbType {
	case config.DBPostgres, config.DBMariaDB:
		password, err := p.ensurePassword(svcName)
		if err != nil {
			return fmt.Errorf("generating password for %s: %w", svcName, err)
		}
		if dbType == config.DBPostgres && decl.ReadOnly {
			return p.provisionPostgresReadOnly(ctx, svcName, decl, password)
		}
		if dbType == config.DBPostgres {
			return p.provisionPostgres(ctx, svcName, decl, password)
		}
		return p.provisionMariaDB(ctx, svcName, decl, password)
	case config.DBRedis:
		return p.provisionRedis(svcName, decl)
	case config.DBS3:
		return p.provisionGarage(svcName, decl)
	default:
		return fmt.Errorf("unsupported database type: %s", dbType)
	}
}

// Deprovision undoes Provision's access grant, keeping data: the SQL engines
// drop the role, Garage deletes the access key (the bucket and its objects
// stay), and redis frees the database number for reuse.
func (p *Provisioner) Deprovision(ctx context.Context, dbType config.DBType, svcName string, decl config.ServiceDBDecl) error {
	switch dbType {
	case config.DBPostgres:
		if decl.ReadOnly {
			return p.deprovisionPostgresReadOnly(decl)
		}
		return p.deprovisionPostgres(ctx, decl)
	case config.DBMariaDB:
		return p.deprovisionMariaDB(ctx, decl)
	case config.DBRedis:
		return config.ReleaseRedisDB(p.ConfigDir, svcName, decl.Name)
	case config.DBS3:
		return p.deprovisionGarage(svcName)
	default:
		return fmt.Errorf("unsupported database type: %s", dbType)
	}
}

// Bootstrap does the one-time setup a freshly started shared instance needs
// before anything can be provisioned on it. Only Garage has any: a new node
// has no cluster layout and refuses every write until one is applied.
func (p *Provisioner) Bootstrap(ctx context.Context, dbType config.DBType) error {
	if dbType == config.DBS3 {
		return p.ensureGarageLayout()
	}
	return nil
}

// EnsureRunning makes sure the shared instance for dbType has a running
// container on this node. With a Deployer it starts the provider when it has
// none (callers then WaitHealthy); without one it only checks, and says how
// to start it.
func (p *Provisioner) EnsureRunning(ctx context.Context, dbType config.DBType) error {
	provider := config.SharedDBName(dbType)
	if provider == "" {
		return fmt.Errorf("unknown database type: %s", dbType)
	}
	_, err := p.Container(dbType)
	if err == nil {
		return nil
	}
	if p.Deployer == nil {
		return fmt.Errorf("%w\n  Start: homelab up %s", err, provider)
	}
	if err := p.Deployer.Up(ctx, provider); err != nil {
		return fmt.Errorf("starting %s: %w", provider, err)
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
// Deploying a provider returns as soon as Swarm accepts the spec, before its
// task container even exists, let alone before Postgres has finished recovery
// and is accepting connections. Starting a dependent service in that window
// gives it connection-refused errors that look like misconfiguration, so
// callers that auto-start a dependency must wait here first. The container
// is re-resolved on every poll: a task that fails is replaced by a new one.
//
// A container with no healthcheck reports no health state at all; for those,
// "running" is the best signal available and is accepted.
func (p *Provisioner) WaitHealthy(ctx context.Context, dbType config.DBType, timeout time.Duration) error {
	provider := config.SharedDBName(dbType)
	if provider == "" {
		return fmt.Errorf("unknown database type: %s", dbType)
	}

	interval := p.PollInterval
	if interval <= 0 {
		interval = healthPollInterval
	}

	deadline := time.Now().Add(timeout)
	var last string
	for {
		container, err := p.Container(dbType)
		if err != nil {
			last = err.Error()
		} else {
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
		}

		if !time.Now().Before(deadline) {
			if last == "" {
				last = "no status reported"
			}
			return fmt.Errorf("%s did not become healthy within %s (last status: %s)\n"+
				"  Check: homelab logs %s\n"+
				"  If it never starts, confirm `homelab setup %s` has been run — "+
				"the image needs its root password to initialise",
				provider, timeout, last, provider, provider)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// swarmServiceLabel is the label Swarm puts on a task's container naming
// its service.
const swarmServiceLabel = "com.docker.swarm.service.name"

// Container returns the container to `docker exec` into for dbType's shared
// instance: ContainerFor when set, else the running Swarm task on this node.
func (p *Provisioner) Container(dbType config.DBType) (string, error) {
	if p.ContainerFor != nil {
		return p.ContainerFor(dbType)
	}
	return p.taskContainer(dbType)
}

// taskContainer finds the running task container of the provider's Swarm
// service on this node. The service is <provider>_<provider>: a stack's
// services are named <stack>_<compose key>, and every provider's compose
// file has one service keyed by its own name (pinned by a test). Providers
// are pinned to core nodes, where the leader runs, so the task is local.
func (p *Provisioner) taskContainer(dbType config.DBType) (string, error) {
	provider := config.SharedDBName(dbType)
	if provider == "" {
		return "", fmt.Errorf("unknown database type: %s", dbType)
	}
	if p.API == nil {
		return "", fmt.Errorf("no Docker API to find the %s container with", provider)
	}
	list, err := p.API.ContainerList(context.Background(), container.ListOptions{Filters: filters.NewArgs(
		filters.Arg("label", swarmServiceLabel+"="+provider+"_"+provider),
		filters.Arg("status", "running"),
	)})
	if err != nil {
		return "", fmt.Errorf("finding the %s container: %w", provider, err)
	}
	if len(list) == 0 {
		return "", fmt.Errorf("%s is not running on this node", provider)
	}
	// During a rolling update the old task can still be running beside its
	// replacement; the newest is the one that stays.
	newest := slices.MaxFunc(list, func(a, b container.Summary) int { return cmp.Compare(a.Created, b.Created) })
	return newest.ID, nil
}

func (p *Provisioner) ensurePassword(svcName string) (string, error) {
	key := config.DBPasswordKey(svcName)
	if p.SM != nil {
		// A read error is not "no password yet": minting a fresh one here would
		// ALTER the live role to a password the running service does not have.
		existing, err := p.SM.Get("", key)
		if err != nil {
			return "", fmt.Errorf("reading existing password from keyring: %w", err)
		}
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

// execSQL runs sql as user, feeding it over stdin rather than argv: statements
// here carry passwords, and a process's argv is readable by anyone on the host
// via ps or /proc. The client follows dbType: a Swarm task's container ID
// says nothing about the engine inside.
func (p *Provisioner) execSQL(dbType config.DBType, container, user, sql string) error {
	var args []string
	if dbType == config.DBPostgres {
		args = []string{"exec", "-i", container, "psql", "-U", user, "-v", "ON_ERROR_STOP=1", "-f", "-"}
	} else {
		args = []string{"exec", "-i", container, "mysql", "-u", "root"}
	}
	return p.RC.RunFrom(strings.NewReader(sql), "docker", args...)
}

// execPSQL runs a psql command with database context and returns output.
func (p *Provisioner) execPSQL(container, db, user string, args ...string) ([]byte, error) {
	psqlArgs := []string{"exec", container, "psql", "-U", user, "-d", db, "-t", "-A"}
	psqlArgs = append(psqlArgs, args...)
	return p.RC.Output("docker", psqlArgs...)
}

func (p *Provisioner) provisionPostgres(ctx context.Context, svcName string, decl config.ServiceDBDecl, password string) error {
	container, err := p.Container(config.DBPostgres)
	if err != nil {
		return err
	}

	// Ownership model: one database per service, owned outright by that
	// service's role. Every database here is single-tenant, so an owner role is
	// both simpler and closer to what upstream images expect than the
	// grant-only setup this replaced: the service can create its own schemas,
	// types and extensions without further privileges, and pg_dump/pg_restore
	// round-trip without --no-owner. The role is therefore created before the
	// database, so CREATE DATABASE can name it as OWNER.

	// 1. Create the role, or reset its password if it is already there.
	if err := p.ensureLoginRole(container, decl.User, password); err != nil {
		return err
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
	out, err := p.execPSQL(container, "postgres", "postgres",
		"-c", fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname=%s", escLit(decl.Database)))
	if err != nil {
		return fmt.Errorf("checking for database %s: %w", decl.Database, err)
	}
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

// ensureLoginRole creates user as a login role with password, or resets the
// password of an existing one.
// PostgreSQL has no CREATE USER IF NOT EXISTS — unlike MySQL, that spelling
// is a plain syntax error (see the CREATE ROLE grammar), so the role has to
// be looked up first. ALTER on the existing-role path also re-syncs the
// password with the keyring, making a repeated `homelab setup` idempotent.
// A failed probe must not be read as "absent": the CREATE that follows would
// fail, and postgres logs the failing statement, password included.
func (p *Provisioner) ensureLoginRole(container, user, password string) error {
	out, err := p.execPSQL(container, "postgres", "postgres",
		"-c", fmt.Sprintf("SELECT 1 FROM pg_roles WHERE rolname=%s", escLit(user)))
	if err != nil {
		return fmt.Errorf("checking for user %s: %w", user, err)
	}
	verb := "CREATE"
	if strings.TrimSpace(string(out)) != "" {
		verb = "ALTER"
	}
	if err := p.execSQL(config.DBPostgres, container, "postgres", fmt.Sprintf("%s USER %s WITH LOGIN PASSWORD %s;",
		verb, escID(user), escLit(password))); err != nil {
		return fmt.Errorf("creating user %s: %w", user, err)
	}
	return nil
}

// pgDatabaseExists reports whether database exists on the shared instance.
func (p *Provisioner) pgDatabaseExists(container, database string) (bool, error) {
	out, err := p.execPSQL(container, "postgres", "postgres",
		"-c", fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname=%s", escLit(database)))
	if err != nil {
		return false, fmt.Errorf("checking for database %s: %w", database, err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// execSQLIn runs sql as postgres inside database, over stdin like execSQL.
func (p *Provisioner) execSQLIn(container, database, sql string) error {
	return p.RC.RunFrom(strings.NewReader(sql), "docker",
		"exec", "-i", container, "psql", "-U", "postgres", "-d", database, "-v", "ON_ERROR_STOP=1", "-f", "-")
}

// ErrDatabaseMissing matches the error provisioning a read-only declaration
// returns when the other service's database does not exist yet: only `up`
// of that service fixes it, not `setup` of the consumer.
var ErrDatabaseMissing = errors.New("database does not exist yet")

type missingDatabaseError string

// Catalog and custom services name their database after themselves.
func (e missingDatabaseError) Error() string {
	return fmt.Sprintf("database %s does not exist yet — `homelab up %s` first", string(e), string(e))
}

func (e missingDatabaseError) Is(target error) bool { return target == ErrDatabaseMissing }

// checkReadOnlyRole reports whether user exists, and refuses an existing
// role that is not a plain login role: one with superuser, createrole,
// createdb, replication or bypassrls, owning a database, a member of another
// role, a role other roles are members of (a group role: granting it SELECT
// would hand that read access to every member), or owning a relation or
// schema in database (checked when dbExists). Taking such a role over would
// reset another service's (or the admin's) credential, and dropping it would
// destroy what it holds.
func (p *Provisioner) checkReadOnlyRole(container, user, database string, dbExists bool) (bool, error) {
	out, err := p.execPSQL(container, "postgres", "postgres", "-c", fmt.Sprintf(
		"SELECT r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls, "+
			"EXISTS (SELECT 1 FROM pg_database d WHERE d.datdba = r.oid), "+
			"EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member = r.oid), "+
			"EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = r.oid) "+
			"FROM pg_roles r WHERE r.rolname=%s", escLit(user)))
	if err != nil {
		return false, fmt.Errorf("checking role %s: %w", user, err)
	}
	row := strings.TrimSpace(string(out))
	if row == "" {
		return false, nil
	}
	cols := strings.Split(row, "|")
	if len(cols) != 4 {
		return true, fmt.Errorf("checking role %s: unexpected result %q", user, row)
	}
	var why []string
	if cols[0] != "f" {
		why = append(why, "has superuser, createrole, createdb, replication or bypassrls")
	}
	if cols[1] != "f" {
		why = append(why, "owns a database")
	}
	if cols[2] != "f" {
		why = append(why, "is a member of another role")
	}
	if cols[3] != "f" {
		why = append(why, "has members: it is a group role")
	}
	if dbExists {
		out, err := p.execPSQL(container, database, "postgres", "-c", fmt.Sprintf(
			"SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_roles r ON c.relowner = r.oid WHERE r.rolname=%[1]s) "+
				"OR EXISTS (SELECT 1 FROM pg_namespace n JOIN pg_roles r ON n.nspowner = r.oid WHERE r.rolname=%[1]s)",
			escLit(user)))
		if err != nil {
			return true, fmt.Errorf("checking what role %s owns in %s: %w", user, database, err)
		}
		if strings.TrimSpace(string(out)) != "f" {
			why = append(why, "owns tables or schemas in "+database)
		}
	}
	if len(why) > 0 {
		return true, fmt.Errorf("role %s already exists and is not a plain login role (%s); a read-only declaration needs a role of its own",
			user, strings.Join(why, ", "))
	}
	return true, nil
}

// provisionPostgresReadOnly gives the consumer svcName a login role that can
// read another service's database and nothing more: no CREATE, no ownership.
// The database belongs to its own service, so a missing one is an error, not
// something to create. Tables the owner creates later are covered by default
// privileges.
func (p *Provisioner) provisionPostgresReadOnly(_ context.Context, svcName string, decl config.ServiceDBDecl, password string) error {
	container, err := p.Container(config.DBPostgres)
	if err != nil {
		return err
	}
	exists, err := p.pgDatabaseExists(container, decl.Database)
	if err != nil {
		return err
	}
	if !exists {
		return missingDatabaseError(decl.Database)
	}
	out, err := p.execPSQL(container, "postgres", "postgres",
		"-c", fmt.Sprintf("SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=%s", escLit(decl.Database)))
	if err != nil {
		return fmt.Errorf("finding the owner of database %s: %w", decl.Database, err)
	}
	owner := strings.TrimSpace(string(out))
	if owner == "" {
		return fmt.Errorf("finding the owner of database %s: no result", decl.Database)
	}
	// Resetting an existing role's password here would hand svcName that
	// role's login (the owner's, a superuser's): only a plain role, or a new
	// one, may become a read-only role.
	if _, err := p.checkReadOnlyRole(container, decl.User, decl.Database, true); err != nil {
		return fmt.Errorf("read-only access for %s: %w", svcName, err)
	}

	if err := p.ensureLoginRole(container, decl.User, password); err != nil {
		return err
	}
	ro, db := escID(decl.User), escID(decl.Database)
	sql := fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s;\n"+
		"GRANT USAGE ON SCHEMA public TO %s;\n"+
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO %s;\n"+
		"GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO %s;\n"+
		"ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT ON TABLES TO %s;\n",
		db, ro, ro, ro, ro, escID(owner), ro)
	if err := p.execSQLIn(container, decl.Database, sql); err != nil {
		return fmt.Errorf("granting read-only access on %s to %s: %w", decl.Database, decl.User, err)
	}
	return nil
}

// deprovisionPostgresReadOnly drops a read-only role. Its grants and default
// privileges live in the other service's database and block DROP ROLE, so
// DROP OWNED BY clears them there first — it owns nothing, so nothing of
// that database's data goes with it. A database already gone leaves only
// the role; a role never created (the consumer never got past "up the owner
// first") leaves nothing, and DROP OWNED BY would fail on it. A role that is
// not a plain login role was never ours to drop and is left alone.
func (p *Provisioner) deprovisionPostgresReadOnly(decl config.ServiceDBDecl) error {
	container, err := p.Container(config.DBPostgres)
	if err != nil {
		return err
	}
	exists, err := p.pgDatabaseExists(container, decl.Database)
	if err != nil {
		return err
	}
	roleExists, err := p.checkReadOnlyRole(container, decl.User, decl.Database, exists)
	if err != nil {
		return fmt.Errorf("%w; left alone, not dropped", err)
	}
	if !roleExists {
		return nil
	}
	if exists {
		if err := p.execSQLIn(container, decl.Database, fmt.Sprintf("DROP OWNED BY %s;", escID(decl.User))); err != nil {
			return fmt.Errorf("revoking %s's access to %s: %w", decl.User, decl.Database, err)
		}
	}
	if err := p.execSQL(config.DBPostgres, container, "postgres", fmt.Sprintf("DROP ROLE IF EXISTS %s;", escID(decl.User))); err != nil {
		return fmt.Errorf("dropping role %s: %w", decl.User, err)
	}
	return nil
}

func (p *Provisioner) deprovisionPostgres(ctx context.Context, decl config.ServiceDBDecl) error {
	container, err := p.Container(config.DBPostgres)
	if err != nil {
		return err
	}
	sql := fmt.Sprintf(`DROP USER IF EXISTS %s;`, escID(decl.User))
	return p.execSQL(config.DBPostgres, container, "postgres", sql)
}

// mariaAnyHost is MariaDB's "connect from anywhere" host pattern. Provision and
// deprovision must spell it identically: the host is stored verbatim as part of
// the account identity, so creating user@'%%' and later dropping user@'%' leaves
// the account (and its grants) behind. Keeping it in one constant is what stops
// the two from drifting again — the previous code wrote '%%%%' in one Sprintf
// format and '%%' in the other, which render as '%%' and '%'.
const mariaAnyHost = "%"

func (p *Provisioner) provisionMariaDB(ctx context.Context, svcName string, decl config.ServiceDBDecl, password string) error {
	container, err := p.Container(config.DBMariaDB)
	if err != nil {
		return err
	}
	db := escMariaID(decl.Database)
	account := escMariaLit(decl.User) + "@" + escMariaLit(mariaAnyHost)
	pw := escMariaLit(password)
	// CREATE … IF NOT EXISTS leaves an existing account's password alone, so the
	// ALTER is what re-syncs it with the keyring on a repeated `homelab setup`.
	sql := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s; "+
		"CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s; "+
		"ALTER USER %s IDENTIFIED BY %s; "+
		"GRANT ALL PRIVILEGES ON %s.* TO %s; "+
		"FLUSH PRIVILEGES;",
		db, account, pw, account, pw, db, account)
	return p.execSQL(config.DBMariaDB, container, "root", sql)
}

func (p *Provisioner) deprovisionMariaDB(ctx context.Context, decl config.ServiceDBDecl) error {
	container, err := p.Container(config.DBMariaDB)
	if err != nil {
		return err
	}
	sql := fmt.Sprintf("DROP USER IF EXISTS %s@%s;", escMariaLit(decl.User), escMariaLit(mariaAnyHost))
	return p.execSQL(config.DBMariaDB, container, "root", sql)
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

// escMariaID wraps a MariaDB identifier in backticks, doubling any embedded
// backtick.
func escMariaID(id string) string {
	return "`" + strings.ReplaceAll(id, "`", "``") + "`"
}

// escMariaLit renders a MariaDB single-quoted string literal. Unlike escLit it
// also escapes backslashes, which MariaDB treats as an escape character inside
// string literals unless NO_BACKSLASH_ESCAPES is set.
func escMariaLit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
