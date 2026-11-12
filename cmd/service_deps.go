package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/db"
	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
)

// Shared-database dependency startup. A service that declares a shared
// postgres/mariadb needs that container up and provisioned before its own
// compose run, which is a different concern from running the service itself.

// sharedDBStartTimeout bounds how long we wait for an auto-started shared
// database to become healthy. Postgres recovery after an unclean shutdown is the
// slow case; a minute is generous without hanging a script forever.
const sharedDBStartTimeout = 90 * time.Second

// ensureDBDependencies makes sure every shared database a service declares is
// actually usable before that service starts.
//
// A declared dependency is a dependency: if the shared container is not running
// we start it and wait for it to report healthy, rather than failing and telling
// the user to run a command we could have run ourselves. Compose cannot express
// this — the shared databases live in their own compose projects, so
// `depends_on` cannot reach them.
func ensureDBDependencies(ctx context.Context, root, name string) error {
	svcCfg, err := config.Load(config.ServiceConfigFile(root, name))
	if err != nil {
		return err
	}
	if svcCfg == nil || svcCfg.Databases.Kind == 0 {
		return nil
	}

	svcDB, err := svcCfg.ServiceDatabases()
	if err != nil {
		return fmt.Errorf("reading database declarations: %w", err)
	}
	if len(svcDB) == 0 {
		return nil
	}
	for i := range svcDB {
		if entry := &svcDB[i]; entry.Type == config.DBPostgres && entry.ReadOnly {
			if other := readOnlyUserDeclarer(root, name, entry.User); other != "" {
				return fmt.Errorf("read-only user %q is already declared by %s: give %s's declaration a user of its own",
					entry.User, other, name)
			}
		}
	}

	// Sorted so the startup order (and any output) is deterministic.
	types := make([]config.DBType, 0, len(svcDB.DBTypeSet()))
	for dbType := range svcDB.DBTypeSet() {
		types = append(types, dbType)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })

	// A real secrets manager, not nil: provisioning stores the generated role
	// password in the keyring, and the same password is what buildEnv injects
	// into the service.
	sm, err := secrets.Open()
	if err != nil {
		return fmt.Errorf("opening keyring: %w", err)
	}
	p := db.New(root, sm)
	closeAPI, err := attachDocker(p)
	if err != nil {
		return err
	}
	defer closeAPI()
	p.Deployer = swarmStarter{root}

	for _, dbType := range types {
		// A service desired running needs its shared database running too:
		// recorded, the agent brings it up after a reboot like any other
		// service.
		recordService(root, config.SharedDBName(dbType), desired.Running)
		_, notRunning := p.Container(dbType)
		if notRunning != nil {
			if err := prepareSharedDB(root, dbType, p); err != nil {
				return err
			}
		}
		// Deploys the provider when it has no running container here.
		if err := p.EnsureRunning(ctx, dbType); err != nil {
			return err
		}
		// Running is not ready. Right after the shared instance starts — a
		// login restore brings it up moments before its dependents — it
		// refuses connections until healthy, and provisioning would fail.
		if err := p.WaitHealthy(ctx, dbType, sharedDBStartTimeout); err != nil {
			return err
		}
		if notRunning != nil {
			shared := config.SharedDBName(dbType)
			if err := p.Bootstrap(ctx, dbType); err != nil {
				return fmt.Errorf("initialising %s: %w", shared, err)
			}
			fmt.Printf("  %s %s is ready\n", styles.Success.Render("✓"), shared)
		}
	}

	// Provision the role and database too, not just the container.
	//
	// Starting the shared instance but leaving the service's role uncreated is
	// the worst of both worlds: buildEnv fills in user/database/host from the
	// declaration regardless, so the service starts against credentials that
	// were never created and crash-loops on "failed to connect to
	// user=<svc> database=<svc>" — which reads like a network fault, not a
	// missing setup step. Provisioning is idempotent and needs no input, so
	// there is nothing to ask the user for.
	for i := range svcDB {
		entry := &svcDB[i]
		if err := p.Provision(ctx, entry.Type, name, entry.ServiceDBDecl); err != nil {
			if errors.Is(err, db.ErrDatabaseMissing) {
				// Another service's database: setup here cannot create it.
				return fmt.Errorf("provisioning %s database %q for %s: %w", entry.Type, entry.Database, name, err)
			}
			return fmt.Errorf(
				"provisioning %s database %q for %s: %w\n"+
					"  Run `homelab setup %s` to configure it interactively",
				entry.Type, entry.Database, name, err, name)
		}
	}
	return nil
}

// prepareSharedDB readies a shared instance that is about to be started:
// installed, announced, its generated secrets minted. Installation is left
// to the user: the shared databases need a root password in the keyring
// before they will initialise, so silently installing one would just
// produce a container that crash-loops.
func prepareSharedDB(root string, dbType config.DBType, p *db.Provisioner) error {
	shared := config.SharedDBName(dbType)
	composeFile := run.ServiceComposeFile(root, shared)
	if _, err := os.Stat(composeFile); err != nil {
		return fmt.Errorf("%s is required by this service but is not installed\n"+
			"  Install: homelab add %s && homelab setup %s", shared, shared, shared)
	}
	fmt.Printf("%s Starting %s (required by this service)…\n",
		styles.Primary.Render("→"), styles.Bold.Render(shared))
	return ensureGeneratedSecrets(p, shared)
}

// swarmStarter lets the provisioner deploy a shared database provider the
// way `homelab up` does.
type swarmStarter struct{ root string }

func (s swarmStarter) Up(_ context.Context, svc string) error {
	return deployRunning(s.root, svc, (*swarm.Deployer).Up)
}

// attachDocker gives p the Docker API its default container resolver uses
// to find the shared instances' Swarm task containers. Call the returned
// func when done with p.
func attachDocker(p *db.Provisioner) (func(), error) {
	api, err := newSwarmAPI()
	if err != nil {
		return nil, fmt.Errorf("connecting to docker: %w", err)
	}
	p.API = api
	return func() {
		if c, ok := api.(io.Closer); ok {
			_ = c.Close()
		}
	}, nil
}

// ensureGeneratedSecrets mints the secrets a service's config.yaml marks
// `generate:` (the shared redis password, Garage's rpc secret) before its
// containers are created with them.
func ensureGeneratedSecrets(p *db.Provisioner, name string) error {
	created, err := p.EnsureGeneratedSecrets(name)
	for _, k := range created {
		fmt.Printf("  %s generated %s/%s (stored in keyring)\n", styles.Success.Render("✓"), name, k)
	}
	if err != nil {
		return fmt.Errorf("generating secrets for %s: %w", name, err)
	}
	return nil
}

// prepareService is what `up` does before compose for any service: generate
// its own secrets, then start and provision what it depends on.
// provisionDBs is ensureDBDependencies; a var so tests can observe it.
var provisionDBs = ensureDBDependencies

func prepareService(ctx context.Context, root, name string) error {
	sm, err := secrets.Open()
	if err != nil {
		return fmt.Errorf("opening keyring: %w", err)
	}
	if err := ensureGeneratedSecrets(db.New(root, sm), name); err != nil {
		return err
	}
	return provisionDBs(ctx, root, name)
}

// bootstrapIfShared finishes bringing up a shared service started directly
// with `homelab up <name>`: wait for it, then do its one-time initialisation
// (Garage's cluster layout) so the first consumer does not have to.
func bootstrapIfShared(ctx context.Context, root, name string) error {
	dbType, ok := config.IsSharedDBService(name)
	if !ok || dbType != config.DBS3 {
		return nil
	}
	p := db.New(root, nil)
	closeAPI, err := attachDocker(p)
	if err != nil {
		return err
	}
	defer closeAPI()
	if err := p.WaitHealthy(ctx, dbType, sharedDBStartTimeout); err != nil {
		return err
	}
	if err := p.Bootstrap(ctx, dbType); err != nil {
		return fmt.Errorf("initialising %s: %w", name, err)
	}
	return nil
}

// releaseDBDependencies gives back what a deleted service held on the shared
// instances that would otherwise leak: its redis database numbers, its
// Garage access key and its read-only postgres roles on other services'
// databases. Data stays — the bucket, like a postgres database, is only
// removed by hand; the service's own postgres role is kept too, as before.
// Best effort: a stopped garage or postgres only means the key or role
// outlives the service, which is reported, not fatal.
func releaseDBDependencies(ctx context.Context, root, name string) []error {
	svcCfg, err := config.Load(config.ServiceConfigFile(root, name))
	if err != nil || svcCfg == nil || svcCfg.Databases.Kind == 0 {
		return nil
	}
	svcDB, err := svcCfg.ServiceDatabases()
	if err != nil {
		return []error{err}
	}
	// Only the garage key needs the keyring and Docker; the redis numbers
	// are a file here, released even when neither is available.
	p := db.New(root, nil)
	var garageErr, dockerErr error
	if sm, err := openSecrets(); err != nil {
		garageErr = fmt.Errorf("opening keyring: %w", err)
	} else {
		p.SM = sm
	}
	if closeAPI, err := attachDocker(p); err != nil { // no Deployer: deleting never starts garage
		dockerErr = err
		garageErr = errors.Join(garageErr, err)
	} else {
		defer closeAPI()
	}
	var errs []error
	for i := range svcDB {
		entry := &svcDB[i]
		switch entry.Type {
		case config.DBRedis:
		case config.DBPostgres:
			if !entry.ReadOnly {
				continue
			}
			if other := readOnlyUserDeclarer(root, name, entry.User); other != "" {
				fmt.Printf("  read-only role %s kept: %s declares it too\n", entry.User, other)
				continue
			}
			if dockerErr != nil {
				errs = append(errs, fmt.Errorf("read-only role %s for %s not dropped: %w", entry.User, name, dockerErr))
				continue
			}
		case config.DBS3:
			if garageErr == nil {
				garageErr = p.EnsureRunning(ctx, entry.Type)
			}
			if garageErr != nil {
				errs = append(errs, fmt.Errorf("garage key for %s not deleted: %w", name, garageErr))
				continue
			}
		default:
			continue
		}
		if err := deprovisionDB(p, ctx, entry.Type, name, entry.ServiceDBDecl); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// readOnlyUserDeclarer is the first installed service other than name whose
// config.yaml declares a read-only postgres user named user, "" when none
// does. A read-only role belongs to one consumer: a second one declaring it
// would reset its password, and deleting either would drop it under the
// other. An unreadable config.yaml is skipped: it declares nothing usable.
func readOnlyUserDeclarer(root, name, user string) string {
	des, err := os.ReadDir(filepath.Join(root, "services"))
	if err != nil {
		return ""
	}
	for _, de := range des { // sorted by name
		if !de.IsDir() || de.Name() == name {
			continue
		}
		cfg, err := config.Load(config.ServiceConfigFile(root, de.Name()))
		if err != nil || cfg == nil || cfg.Databases.Kind == 0 {
			continue
		}
		decls, err := cfg.ServiceDatabases()
		if err != nil {
			continue
		}
		for _, d := range decls {
			if d.Type == config.DBPostgres && d.ReadOnly && d.User == user {
				return de.Name()
			}
		}
	}
	return ""
}

// deprovisionDB is (*db.Provisioner).Deprovision; a var so tests can observe
// what a delete releases without a database to run it against.
var deprovisionDB = (*db.Provisioner).Deprovision
