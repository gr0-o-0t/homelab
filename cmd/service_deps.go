package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/db"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/secrets"
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

	for _, dbType := range types {
		if err := p.EnsureRunning(ctx, dbType); err != nil {
			if err := startSharedDB(ctx, root, dbType, p); err != nil {
				return err
			}
			continue
		}
		// Running is not ready. Right after the shared instance starts — a
		// login restore brings it up moments before its dependents — it
		// refuses connections until healthy, and provisioning would fail.
		if err := p.WaitHealthy(ctx, dbType, sharedDBStartTimeout); err != nil {
			return err
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
			return fmt.Errorf(
				"provisioning %s database %q for %s: %w\n"+
					"  Run `homelab setup %s` to configure it interactively",
				entry.Type, entry.Database, name, err, name)
		}
	}
	return nil
}

// startSharedDB brings up the shared instance for dbType and waits for it to be
// healthy. Installation is left to the user: the shared databases need a root
// password in the keyring before they will initialise, so silently installing
// one would just produce a container that crash-loops.
func startSharedDB(ctx context.Context, root string, dbType config.DBType, p *db.Provisioner) error {
	shared := config.SharedDBName(dbType)
	composeFile := run.ServiceComposeFile(root, shared)
	if _, err := os.Stat(composeFile); err != nil {
		return fmt.Errorf("%s is required by this service but is not installed\n"+
			"  Install: homelab add %s && homelab setup %s", shared, shared, shared)
	}

	fmt.Printf("%s Starting %s (required by this service)…\n",
		styles.Primary.Render("→"), styles.Bold.Render(shared))

	if err := ensureGeneratedSecrets(p, shared); err != nil {
		return err
	}
	if err := run.Default().DockerComposeEnv(
		composeFile,
		buildEnv(root, shared),
		"up", "-d",
	); err != nil {
		return fmt.Errorf("starting %s: %w", shared, err)
	}

	if err := p.WaitHealthy(ctx, dbType, sharedDBStartTimeout); err != nil {
		return err
	}
	if err := p.Bootstrap(ctx, dbType); err != nil {
		return fmt.Errorf("initialising %s: %w", shared, err)
	}
	fmt.Printf("  %s %s is ready\n", styles.Success.Render("✓"), shared)
	return nil
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
func prepareService(ctx context.Context, root, name string) error {
	sm, err := secrets.Open()
	if err != nil {
		return fmt.Errorf("opening keyring: %w", err)
	}
	if err := ensureGeneratedSecrets(db.New(root, sm), name); err != nil {
		return err
	}
	return ensureDBDependencies(ctx, root, name)
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
	if err := p.WaitHealthy(ctx, dbType, sharedDBStartTimeout); err != nil {
		return err
	}
	if err := p.Bootstrap(ctx, dbType); err != nil {
		return fmt.Errorf("initialising %s: %w", name, err)
	}
	return nil
}

// releaseDBDependencies gives back what a deleted service held on the shared
// instances that would otherwise leak: its redis database numbers and its
// Garage access key. Data stays — the bucket, like a postgres database, is
// only removed by hand. Best effort: a stopped garage only means the key
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
	sm, err := secrets.Open()
	if err != nil {
		return []error{fmt.Errorf("opening keyring: %w", err)}
	}
	p := db.New(root, sm)
	var errs []error
	for i := range svcDB {
		entry := &svcDB[i]
		switch entry.Type {
		case config.DBRedis:
		case config.DBS3:
			if err := p.EnsureRunning(ctx, entry.Type); err != nil {
				errs = append(errs, fmt.Errorf("garage key for %s not deleted: %w", name, err))
				continue
			}
		default:
			continue
		}
		if err := p.Deprovision(ctx, entry.Type, name, entry.ServiceDBDecl); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
